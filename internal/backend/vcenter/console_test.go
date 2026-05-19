package vcenter

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
)

type fakeConsoleLogTailer struct {
	responses []fakeConsoleLogTailResponse
	calls     int
}

type fakeConsoleLogTailResponse struct {
	data       string
	nextOffset int64
	err        error
	block      bool
}

func (f *fakeConsoleLogTailer) Tail(ctx context.Context, _ string, offset int64) ([]byte, int64, error) {
	if f.calls >= len(f.responses) {
		return nil, offset, nil
	}
	response := f.responses[f.calls]
	f.calls++
	if response.block {
		<-ctx.Done()
		return nil, offset, ctx.Err()
	}
	if response.err != nil {
		return nil, offset, response.err
	}
	nextOffset := response.nextOffset
	if nextOffset == 0 {
		nextOffset = offset + int64(len(response.data))
	}
	return []byte(response.data), nextOffset, nil
}

func TestDatastoreConsoleStreamerIgnoresMissingLogUntilAvailable(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{err: errors.New("download console.log: 404 Not Found")},
			{data: "installer started\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)

	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("first poll returned error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("missing log poll wrote output %q", out.String())
	}

	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("second poll returned error: %v", err)
	}
	if got, want := out.String(), "installer started\n"; got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}

func TestDatastoreConsoleStreamerStreamsOnlyAppendedBytes(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{data: "line 1\n"},
			{data: "line 2\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)

	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("first poll returned error: %v", err)
	}
	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("second poll returned error: %v", err)
	}

	if got, want := out.String(), "line 1\nline 2\n"; got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}

func TestDatastoreConsoleStreamerFlushesFinalPartialLine(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{data: "line 1\npartial"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)

	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("poll returned error: %v", err)
	}
	if got, want := out.String(), "line 1\n"; got != want {
		t.Fatalf("streamed output before flush = %q, want %q", got, want)
	}
	if err := streamer.writer.Flush(); err != nil {
		t.Fatalf("flush returned error: %v", err)
	}
	if got, want := out.String(), "line 1\npartial\n"; got != want {
		t.Fatalf("streamed output after flush = %q, want %q", got, want)
	}
}

func TestDatastoreConsoleStreamerWarnsWhenNoBytesStreamedOnFinish(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{data: ""},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)

	streamer.finish(context.Background())

	if got := out.String(); !strings.Contains(got, "ended with zero bytes streamed") {
		t.Fatalf("finish output = %q, want zero-byte warning", got)
	}
}

func TestDatastoreConsoleStreamerHandlesTruncatedLog(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{data: "first\nsecond\n"},
			{data: "new\n", nextOffset: 4},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)

	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("first poll returned error: %v", err)
	}
	if err := streamer.poll(context.Background()); err != nil {
		t.Fatalf("second poll returned error: %v", err)
	}

	if got, want := out.String(), "first\nsecond\nnew\n"; got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}

func TestDatastoreConsoleStreamerPollTimeoutDoesNotStopFutureStreaming(t *testing.T) {
	tailer := &fakeConsoleLogTailer{
		responses: []fakeConsoleLogTailResponse{
			{block: true},
			{data: "after timeout\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamerWithTailer(tailer, "console.log", "[ds] console.log", &out)
	streamer.pollTimeout = 10 * time.Millisecond

	streamer.pollAndWarn(context.Background())
	streamer.pollAndWarn(context.Background())

	if got, want := out.String(), "after timeout\n"; got != want {
		t.Fatalf("streamed output = %q, want %q", got, want)
	}
}

func TestDatastoreRangeTailerReadsPartialContent(t *testing.T) {
	tailer := newTestDatastoreRangeTailer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "13")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if got, want := r.Header.Get("Range"), "bytes=7-12"; got != want {
				t.Fatalf("Range header = %q, want %q", got, want)
			}
			w.Header().Set("Content-Range", "bytes 7-12/13")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("second"))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	data, offset, err := tailer.Tail(context.Background(), "console.log", 7)
	if err != nil {
		t.Fatalf("Tail returned error: %v", err)
	}
	if got, want := string(data), "second"; got != want {
		t.Fatalf("Tail data = %q, want %q", got, want)
	}
	if offset != 13 {
		t.Fatalf("Tail offset = %d, want 13", offset)
	}
}

func TestDatastoreRangeTailerFallsBackWhenRangeIgnored(t *testing.T) {
	tailer := newTestDatastoreRangeTailer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "12")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("line1\nline2\n"))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	data, offset, err := tailer.Tail(context.Background(), "console.log", 6)
	if err != nil {
		t.Fatalf("Tail returned error: %v", err)
	}
	if got, want := string(data), "line2\n"; got != want {
		t.Fatalf("Tail data = %q, want %q", got, want)
	}
	if offset != 12 {
		t.Fatalf("Tail offset = %d, want 12", offset)
	}
}

func TestDatastoreRangeTailerHandlesMissingLog(t *testing.T) {
	tailer := newTestDatastoreRangeTailer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, _, err := tailer.Tail(context.Background(), "console.log", 0)
	if err == nil {
		t.Fatal("Tail returned nil error for missing log")
	}
	if !isMissingConsoleLog(err) {
		t.Fatalf("Tail error = %v, want missing log error", err)
	}
}

func TestDatastoreRangeTailerHandlesTruncatedLog(t *testing.T) {
	tailer := newTestDatastoreRangeTailer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if got, want := r.Header.Get("Range"), "bytes=0-3"; got != want {
				t.Fatalf("Range header = %q, want %q", got, want)
			}
			w.Header().Set("Content-Range", "bytes 0-3/4")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("new\n"))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	data, offset, err := tailer.Tail(context.Background(), "console.log", 20)
	if err != nil {
		t.Fatalf("Tail returned error: %v", err)
	}
	if got, want := string(data), "new\n"; got != want {
		t.Fatalf("Tail data = %q, want %q", got, want)
	}
	if offset != 4 {
		t.Fatalf("Tail offset = %d, want 4", offset)
	}
}

func TestDatastoreRangeTailerFallsBackWhenHeadUnsupported(t *testing.T) {
	tailer := newTestDatastoreRangeTailer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			switch r.Header.Get("Range") {
			case "bytes=0-0":
				w.Header().Set("Content-Range", "bytes 0-0/9")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("i"))
			case "bytes=0-8":
				w.Header().Set("Content-Range", "bytes 0-8/9")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("installer"))
			default:
				t.Fatalf("unexpected Range header %q", r.Header.Get("Range"))
			}
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	data, offset, err := tailer.Tail(context.Background(), "console.log", 0)
	if err != nil {
		t.Fatalf("Tail returned error: %v", err)
	}
	if got, want := string(data), "installer"; got != want {
		t.Fatalf("Tail data = %q, want %q", got, want)
	}
	if offset != 9 {
		t.Fatalf("Tail offset = %d, want 9", offset)
	}
}

func newTestDatastoreRangeTailer(t *testing.T, handler http.HandlerFunc) *datastoreRangeTailer {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &datastoreRangeTailer{source: testDatastoreHTTPSource{baseURL: baseURL, client: server.Client()}}
}

type testDatastoreHTTPSource struct {
	baseURL *url.URL
	client  *http.Client
}

func (s testDatastoreHTTPSource) ServiceTicket(_ context.Context, path string, _ string) (*url.URL, *http.Cookie, error) {
	u := *s.baseURL
	u.Path = "/" + strings.TrimPrefix(path, "/")
	return &u, nil, nil
}

func (s testDatastoreHTTPSource) DownloadRequest(ctx context.Context, u *url.URL, param *soap.Download) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, param.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	for key, value := range param.Headers {
		req.Header.Set(key, value)
	}
	if param.Ticket != nil {
		req.AddCookie(param.Ticket)
	}
	return s.client.Do(req)
}
