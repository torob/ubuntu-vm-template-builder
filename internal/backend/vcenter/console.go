package vcenter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"

	"ubuntu-vm-template-builder/internal/qemulog"
)

const (
	consoleLogPollInterval     = 2 * time.Second
	consoleLogPollTimeout      = 10 * time.Second
	consoleLogFinalPollTimeout = 30 * time.Second
)

type consoleLogTailer interface {
	Tail(ctx context.Context, path string, offset int64) ([]byte, int64, error)
}

type datastoreHTTPSource interface {
	ServiceTicket(ctx context.Context, path string, method string) (*url.URL, *http.Cookie, error)
	DownloadRequest(ctx context.Context, u *url.URL, param *soap.Download) (*http.Response, error)
}

type govmomiDatastoreHTTPSource struct {
	datastore *object.Datastore
	host      *object.HostSystem
}

func (s govmomiDatastoreHTTPSource) ServiceTicket(ctx context.Context, path string, method string) (*url.URL, *http.Cookie, error) {
	if s.host != nil {
		ctx = s.datastore.HostContext(ctx, s.host)
	}
	return s.datastore.ServiceTicket(ctx, path, method)
}

func (s govmomiDatastoreHTTPSource) DownloadRequest(ctx context.Context, u *url.URL, param *soap.Download) (*http.Response, error) {
	return s.datastore.Client().DownloadRequest(ctx, u, param)
}

type datastoreRangeTailer struct {
	source datastoreHTTPSource
}

type datastoreConsoleStreamer struct {
	tailer      consoleLogTailer
	remotePath  string
	displayPath string
	out         io.Writer
	interval    time.Duration
	pollTimeout time.Duration
	writer      *qemulog.CompactingWriter
	offset      int64
	bytesRead   int64
	lastWarning string
}

func newDatastoreConsoleStreamer(datastore *object.Datastore, host *object.HostSystem, remotePath, displayPath string, out io.Writer) *datastoreConsoleStreamer {
	return newDatastoreConsoleStreamerWithTailer(newDatastoreRangeTailer(datastore, host), remotePath, displayPath, out)
}

func newDatastoreConsoleStreamerWithTailer(tailer consoleLogTailer, remotePath, displayPath string, out io.Writer) *datastoreConsoleStreamer {
	if out == nil {
		out = io.Discard
	}
	return &datastoreConsoleStreamer{
		tailer:      tailer,
		remotePath:  remotePath,
		displayPath: displayPath,
		out:         out,
		interval:    consoleLogPollInterval,
		pollTimeout: consoleLogPollTimeout,
		writer:      qemulog.NewCompactingWriter(out),
	}
}

func newDatastoreRangeTailer(datastore *object.Datastore, host *object.HostSystem) *datastoreRangeTailer {
	return &datastoreRangeTailer{source: govmomiDatastoreHTTPSource{datastore: datastore, host: host}}
}

func (s *datastoreConsoleStreamer) start(ctx context.Context) func(context.Context) {
	if s == nil {
		return func(context.Context) {}
	}

	fmt.Printf("Streaming vCenter installer console from datastore log: %s\n", s.displayPath)
	streamCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run(streamCtx)
	}()

	return func(finalCtx context.Context) {
		cancel()
		<-done
		s.finish(finalCtx)
	}
}

func (s *datastoreConsoleStreamer) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		s.pollAndWarn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *datastoreConsoleStreamer) pollAndWarn(ctx context.Context) {
	pollCtx := ctx
	cancel := func() {}
	if s.pollTimeout > 0 {
		pollCtx, cancel = context.WithTimeout(ctx, s.pollTimeout)
	}
	defer cancel()

	if err := s.poll(pollCtx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		msg := err.Error()
		if msg != s.lastWarning {
			fmt.Printf("Warning: could not stream vCenter console log %q: %v\n", s.displayPath, err)
			s.lastWarning = msg
		}
	}
}

func (s *datastoreConsoleStreamer) finish(ctx context.Context) {
	s.pollAndWarn(ctx)
	if err := s.writer.Flush(); err != nil {
		fmt.Printf("Warning: could not flush vCenter console log stream: %v\n", err)
	}
	if s.bytesRead == 0 {
		fmt.Fprintf(s.out, "Warning: vCenter console log %q ended with zero bytes streamed; installer may not have used the serial console\n", s.displayPath)
	}
}

func (s *datastoreConsoleStreamer) poll(ctx context.Context) error {
	chunk, newOffset, err := s.tailer.Tail(ctx, s.remotePath, s.offset)
	if err != nil {
		if isMissingConsoleLog(err) {
			return nil
		}
		return err
	}
	if len(chunk) == 0 {
		s.offset = newOffset
		return nil
	}

	if _, err := s.writer.Write(chunk); err != nil {
		return err
	}
	s.bytesRead += int64(len(chunk))
	s.offset = newOffset
	return nil
}

func (t *datastoreRangeTailer) Tail(ctx context.Context, path string, offset int64) ([]byte, int64, error) {
	size, err := t.fileSize(ctx, path)
	if err != nil {
		return nil, offset, err
	}
	if size < offset {
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}

	return t.readRange(ctx, path, offset, size-1)
}

func (t *datastoreRangeTailer) fileSize(ctx context.Context, path string) (int64, error) {
	size, err := t.headSize(ctx, path)
	if err == nil {
		return size, nil
	}
	if isMissingConsoleLog(err) {
		return 0, err
	}
	return t.rangeProbeSize(ctx, path)
}

func (t *datastoreRangeTailer) headSize(ctx context.Context, path string) (int64, error) {
	res, err := t.request(ctx, path, http.MethodHead, nil)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		size, ok := responseContentLength(res)
		if !ok {
			return 0, fmt.Errorf("HEAD %s returned no content length", path)
		}
		return size, nil
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return 0, fmt.Errorf("HEAD %s: %s", path, res.Status)
	default:
		return 0, datastoreHTTPStatusError{method: http.MethodHead, path: path, statusCode: res.StatusCode, status: res.Status}
	}
}

func (t *datastoreRangeTailer) rangeProbeSize(ctx context.Context, path string) (int64, error) {
	res, err := t.request(ctx, path, http.MethodGet, map[string]string{"Range": "bytes=0-0"})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusPartialContent:
		size, ok := parseContentRangeTotal(res.Header.Get("Content-Range"))
		if !ok {
			return 0, fmt.Errorf("range probe %s returned invalid Content-Range %q", path, res.Header.Get("Content-Range"))
		}
		return size, nil
	case http.StatusOK:
		size, ok := responseContentLength(res)
		if !ok {
			return 0, fmt.Errorf("range probe %s returned full response without content length", path)
		}
		return size, nil
	case http.StatusRequestedRangeNotSatisfiable:
		if size, ok := parseContentRangeTotal(res.Header.Get("Content-Range")); ok {
			return size, nil
		}
		return 0, nil
	default:
		return 0, datastoreHTTPStatusError{method: http.MethodGet, path: path, statusCode: res.StatusCode, status: res.Status}
	}
}

func (t *datastoreRangeTailer) readRange(ctx context.Context, path string, start, end int64) ([]byte, int64, error) {
	rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
	res, err := t.request(ctx, path, http.MethodGet, map[string]string{"Range": rangeHeader})
	if err != nil {
		return nil, start, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusPartialContent:
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, start, err
		}
		return data, start + int64(len(data)), nil
	case http.StatusOK:
		data, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, start, err
		}
		if int64(len(data)) < start {
			return data, int64(len(data)), nil
		}
		chunk := data[start:]
		return chunk, start + int64(len(chunk)), nil
	case http.StatusRequestedRangeNotSatisfiable:
		if size, ok := parseContentRangeTotal(res.Header.Get("Content-Range")); ok {
			return nil, size, nil
		}
		return nil, start, nil
	default:
		return nil, start, datastoreHTTPStatusError{method: http.MethodGet, path: path, statusCode: res.StatusCode, status: res.Status}
	}
}

func (t *datastoreRangeTailer) request(ctx context.Context, path, method string, headers map[string]string) (*http.Response, error) {
	u, ticket, err := t.source.ServiceTicket(ctx, path, method)
	if err != nil {
		return nil, err
	}
	param := &soap.Download{
		Method:  method,
		Headers: headers,
		Ticket:  ticket,
		Close:   ticket != nil,
	}
	return t.source.DownloadRequest(ctx, u, param)
}

type datastoreHTTPStatusError struct {
	method     string
	path       string
	statusCode int
	status     string
}

func (e datastoreHTTPStatusError) Error() string {
	return fmt.Sprintf("%s %s: %s", e.method, e.path, e.status)
}

func isMissingConsoleLog(err error) bool {
	var statusErr datastoreHTTPStatusError
	if errors.As(err, &statusErr) && statusErr.statusCode == http.StatusNotFound {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such file")
}

func responseContentLength(res *http.Response) (int64, bool) {
	if res.ContentLength >= 0 {
		return res.ContentLength, true
	}
	raw := strings.TrimSpace(res.Header.Get("Content-Length"))
	if raw == "" {
		return 0, false
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

func parseContentRangeTotal(header string) (int64, bool) {
	header = strings.TrimSpace(header)
	slash := strings.LastIndex(header, "/")
	if slash < 0 || slash == len(header)-1 {
		return 0, false
	}
	rawTotal := strings.TrimSpace(header[slash+1:])
	if rawTotal == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(rawTotal, 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}
