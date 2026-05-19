package vcenter

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/soap"

	"ubuntu-vm-template-builder/internal/qemulog"
)

const (
	consoleLogPollInterval     = 2 * time.Second
	consoleLogFinalPollTimeout = 30 * time.Second
)

type datastoreDownloader interface {
	Download(ctx context.Context, path string, param *soap.Download) (io.ReadCloser, int64, error)
}

type datastoreConsoleStreamer struct {
	datastore   datastoreDownloader
	remotePath  string
	displayPath string
	out         io.Writer
	interval    time.Duration
	writer      *qemulog.CompactingWriter
	offset      int64
	bytesRead   int64
	lastWarning string
}

func newDatastoreConsoleStreamer(datastore datastoreDownloader, remotePath, displayPath string, out io.Writer) *datastoreConsoleStreamer {
	if out == nil {
		out = io.Discard
	}
	return &datastoreConsoleStreamer{
		datastore:   datastore,
		remotePath:  remotePath,
		displayPath: displayPath,
		out:         out,
		interval:    consoleLogPollInterval,
		writer:      qemulog.NewCompactingWriter(out),
	}
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
	if err := s.poll(ctx); err != nil {
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
	reader, _, err := s.datastore.Download(ctx, s.remotePath, nil)
	if err != nil {
		if isMissingConsoleLog(err) {
			return nil
		}
		return err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}

	if int64(len(data)) < s.offset {
		s.offset = 0
	}
	if int64(len(data)) == s.offset {
		return nil
	}

	chunk := data[s.offset:]
	s.offset = int64(len(data))
	if _, err := s.writer.Write(chunk); err != nil {
		return err
	}
	s.bytesRead += int64(len(chunk))
	return nil
}

func isMissingConsoleLog(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such file")
}
