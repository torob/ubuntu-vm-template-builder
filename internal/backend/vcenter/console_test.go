package vcenter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/soap"
)

type fakeDatastoreDownloader struct {
	responses []fakeDatastoreDownloadResponse
	calls     int
}

type fakeDatastoreDownloadResponse struct {
	data string
	err  error
}

func (f *fakeDatastoreDownloader) Download(context.Context, string, *soap.Download) (io.ReadCloser, int64, error) {
	if f.calls >= len(f.responses) {
		return io.NopCloser(bytes.NewReader(nil)), 0, nil
	}
	response := f.responses[f.calls]
	f.calls++
	if response.err != nil {
		return nil, 0, response.err
	}
	return io.NopCloser(stringsReader(response.data)), int64(len(response.data)), nil
}

func stringsReader(data string) *bytes.Reader {
	return bytes.NewReader([]byte(data))
}

func TestDatastoreConsoleStreamerIgnoresMissingLogUntilAvailable(t *testing.T) {
	downloader := &fakeDatastoreDownloader{
		responses: []fakeDatastoreDownloadResponse{
			{err: errors.New("download console.log: 404 Not Found")},
			{data: "installer started\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamer(downloader, "console.log", "[ds] console.log", &out)

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
	downloader := &fakeDatastoreDownloader{
		responses: []fakeDatastoreDownloadResponse{
			{data: "line 1\n"},
			{data: "line 1\nline 2\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamer(downloader, "console.log", "[ds] console.log", &out)

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
	downloader := &fakeDatastoreDownloader{
		responses: []fakeDatastoreDownloadResponse{
			{data: "line 1\npartial"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamer(downloader, "console.log", "[ds] console.log", &out)

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
	downloader := &fakeDatastoreDownloader{
		responses: []fakeDatastoreDownloadResponse{
			{data: ""},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamer(downloader, "console.log", "[ds] console.log", &out)

	streamer.finish(context.Background())

	if got := out.String(); !strings.Contains(got, "ended with zero bytes streamed") {
		t.Fatalf("finish output = %q, want zero-byte warning", got)
	}
}

func TestDatastoreConsoleStreamerHandlesTruncatedLog(t *testing.T) {
	downloader := &fakeDatastoreDownloader{
		responses: []fakeDatastoreDownloadResponse{
			{data: "first\nsecond\n"},
			{data: "new\n"},
		},
	}
	var out bytes.Buffer
	streamer := newDatastoreConsoleStreamer(downloader, "console.log", "[ds] console.log", &out)

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
