package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"ubuntu-vm-template-builder/internal/qemulog"
)

const (
	UploadContentISO = "iso"
)

var (
	taskPollInterval    = 2 * time.Second
	vmPowerPollInterval = 5 * time.Second
)

type api interface {
	ValidateNode(ctx context.Context, node string) error
	StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error)
	ListVMs(ctx context.Context, node string) ([]VMInfo, error)
	NextID(ctx context.Context) (int, error)
	StorageVolumeExists(ctx context.Context, node, storage, content, volumeID string) (bool, error)
	UploadFile(ctx context.Context, node, storage string, upload StorageUpload) (string, error)
	DeleteVolume(ctx context.Context, node, storage, volumeID string) (string, error)
	CreateVM(ctx context.Context, node string, values url.Values) (string, error)
	StartVM(ctx context.Context, node string, vmid int) (string, error)
	CurrentVMStatus(ctx context.Context, node string, vmid int) (VMStatus, error)
	UpdateVMConfig(ctx context.Context, node string, vmid int, values url.Values) (string, error)
	TemplateVM(ctx context.Context, node string, vmid int) (string, error)
	StopVM(ctx context.Context, node string, vmid int) (string, error)
	DeleteVM(ctx context.Context, node string, vmid int) (string, error)
	WaitTask(ctx context.Context, node, upid string) error
	StreamSerialConsole(ctx context.Context, node string, vmid int, out io.Writer) error
}

type Client struct {
	baseURL     *url.URL
	httpClient  *http.Client
	tokenID     string
	tokenSecret string
	insecure    bool
}

type VMInfo struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Template int    `json:"template"`
}

type VMStatus struct {
	Status string `json:"status"`
}

type StorageStatus struct {
	Active  any           `json:"active"`
	Enabled any           `json:"enabled"`
	Content string        `json:"content"`
	Avail   flexibleInt64 `json:"avail"`
	Used    flexibleInt64 `json:"used"`
	Total   flexibleInt64 `json:"total"`
}

type StorageUpload struct {
	SourcePath string
	FileName   string
	Content    string
}

type taskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

type apiEnvelope struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors"`
}

func NewClient(cfg ConnectionConfig) (*Client, error) {
	baseURL, err := BuildURL(cfg)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &Client{
		baseURL:     baseURL,
		httpClient:  &http.Client{Transport: transport},
		tokenID:     strings.TrimSpace(cfg.TokenID),
		tokenSecret: strings.TrimSpace(cfg.TokenSecret),
		insecure:    cfg.Insecure,
	}, nil
}

func BuildURL(cfg ConnectionConfig) (*url.URL, error) {
	rawHost := strings.TrimSpace(cfg.Host)
	if rawHost == "" {
		return nil, fmt.Errorf("--proxmox-host is required")
	}
	if !strings.Contains(rawHost, "://") {
		rawHost = "https://" + rawHost
	}

	u, err := url.Parse(rawHost)
	if err != nil {
		return nil, fmt.Errorf("parse --proxmox-host: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--proxmox-host must include a host")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/api2/json"
	} else if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/api2/json") {
		u.Path = path.Join(u.Path, "/api2/json")
	}
	u.User = nil
	return u, nil
}

func (c *Client) ValidateNode(ctx context.Context, node string) error {
	var nodeStatus struct {
		Status string `json:"status"`
	}
	if err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/status", url.PathEscape(node)), nil, &nodeStatus); err != nil {
		return fmt.Errorf("validate Proxmox node %q: %w", node, err)
	}
	if nodeStatus.Status != "" && nodeStatus.Status != "online" {
		return fmt.Errorf("Proxmox node %q is %q, want online", node, nodeStatus.Status)
	}
	return nil
}

func (c *Client) StorageStatus(ctx context.Context, node, storage string) (StorageStatus, error) {
	var storageStatus StorageStatus
	if err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/storage/%s/status", url.PathEscape(node), url.PathEscape(storage)), nil, &storageStatus); err != nil {
		return storageStatus, err
	}
	return storageStatus, nil
}

func (c *Client) ListVMs(ctx context.Context, node string) ([]VMInfo, error) {
	var vms []VMInfo
	if err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/qemu", url.PathEscape(node)), nil, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (c *Client) NextID(ctx context.Context) (int, error) {
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	return parseJSONInt(raw)
}

func (c *Client) StorageVolumeExists(ctx context.Context, node, storage, content, volumeID string) (bool, error) {
	var entries []struct {
		VolID string `json:"volid"`
	}
	query := url.Values{}
	if strings.TrimSpace(content) != "" {
		query.Set("content", content)
	}
	if err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage)), query, &entries); err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.VolID == volumeID {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) UploadFile(ctx context.Context, node, storage string, upload StorageUpload) (string, error) {
	source, err := os.Open(upload.SourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("content", upload.Content); err != nil {
		return "", err
	}
	part, err := writer.CreateFormFile("filename", upload.FileName)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, source); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/storage/%s/upload", url.PathEscape(node), url.PathEscape(storage)), nil, writer.FormDataContentType(), &body, &raw); err != nil {
		return "", err
	}
	expectedVolumeID := buildVolumeID(storage, upload.Content, upload.FileName)
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return expectedVolumeID, nil
	}
	if text, ok := parseJSONString(raw); ok && strings.TrimSpace(text) != "" {
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "UPID:") {
			if err := c.WaitTask(ctx, node, text); err != nil {
				return "", fmt.Errorf("wait for storage upload: %w", err)
			}
			return expectedVolumeID, nil
		}
		return text, nil
	}
	var uploadData struct {
		VolID string `json:"volid"`
		UPID  string `json:"upid"`
	}
	if err := json.Unmarshal(raw, &uploadData); err == nil {
		if strings.TrimSpace(uploadData.UPID) != "" {
			if err := c.WaitTask(ctx, node, strings.TrimSpace(uploadData.UPID)); err != nil {
				return "", fmt.Errorf("wait for storage upload: %w", err)
			}
			return expectedVolumeID, nil
		}
		if strings.TrimSpace(uploadData.VolID) != "" {
			return uploadData.VolID, nil
		}
	}
	return expectedVolumeID, nil
}

func (c *Client) DeleteVolume(ctx context.Context, node, storage, volumeID string) (string, error) {
	var raw json.RawMessage
	err := c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(volumeID)), nil, "", nil, &raw)
	if err != nil {
		if isNotFoundError(err) {
			return "", nil
		}
		return "", err
	}
	return parseTaskID(raw), nil
}

func (c *Client) CreateVM(ctx context.Context, node string, values url.Values) (string, error) {
	return c.postTask(ctx, fmt.Sprintf("/nodes/%s/qemu", url.PathEscape(node)), values)
}

func (c *Client) StartVM(ctx context.Context, node string, vmid int) (string, error) {
	return c.postTask(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/start", url.PathEscape(node), vmid), nil)
}

func (c *Client) CurrentVMStatus(ctx context.Context, node string, vmid int) (VMStatus, error) {
	var status VMStatus
	err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/current", url.PathEscape(node), vmid), nil, &status)
	return status, err
}

func (c *Client) UpdateVMConfig(ctx context.Context, node string, vmid int, values url.Values) (string, error) {
	return c.postTask(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), values)
}

func (c *Client) TemplateVM(ctx context.Context, node string, vmid int) (string, error) {
	return c.postTask(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/template", url.PathEscape(node), vmid), nil)
}

func (c *Client) StopVM(ctx context.Context, node string, vmid int) (string, error) {
	return c.postTask(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", url.PathEscape(node), vmid), nil)
}

func (c *Client) DeleteVM(ctx context.Context, node string, vmid int) (string, error) {
	var raw json.RawMessage
	err := c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/nodes/%s/qemu/%d", url.PathEscape(node), vmid), nil, "", nil, &raw)
	if err != nil {
		if isNotFoundError(err) {
			return "", nil
		}
		return "", err
	}
	return parseTaskID(raw), nil
}

func (c *Client) WaitTask(ctx context.Context, node, upid string) error {
	if strings.TrimSpace(upid) == "" {
		return nil
	}
	for {
		var status taskStatus
		err := c.getJSON(ctx, fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), nil, &status)
		if err != nil {
			return err
		}
		if status.Status == "stopped" {
			if status.ExitStatus == "" || status.ExitStatus == "OK" {
				return nil
			}
			return fmt.Errorf("Proxmox task %s failed with exitstatus %s", upid, status.ExitStatus)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

func (c *Client) StreamSerialConsole(ctx context.Context, node string, vmid int, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var proxy struct {
		Ticket string        `json:"ticket"`
		Port   flexibleInt64 `json:"port"`
		UPID   string        `json:"upid"`
	}
	if err := c.postJSON(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/termproxy", url.PathEscape(node), vmid), nil, &proxy); err != nil {
		return err
	}
	if !proxy.Port.Set || proxy.Port.Value <= 0 {
		return fmt.Errorf("Proxmox termproxy returned invalid port %d", proxy.Port.Value)
	}

	wsURL := c.websocketURL(fmt.Sprintf("/nodes/%s/qemu/%d/vncwebsocket", url.PathEscape(node), vmid))
	query := wsURL.Query()
	query.Set("port", strconv.FormatInt(proxy.Port.Value, 10))
	if proxy.Ticket != "" {
		query.Set("vncticket", proxy.Ticket)
	}
	wsURL.RawQuery = query.Encode()

	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment,
	}
	if c.insecure {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	header := http.Header{}
	c.addAuth(header)
	conn, _, err := dialer.DialContext(ctx, wsURL.String(), header)
	if err != nil {
		return err
	}
	defer conn.Close()

	writer := qemulog.NewCompactingWriter(out)
	defer writer.Flush()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if handshake := c.consoleHandshake(proxy.Ticket); handshake != "" {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(handshake)); err != nil {
			return err
		}
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}
		if messageType == websocket.TextMessage || messageType == websocket.BinaryMessage {
			if !bytes.Equal(bytes.TrimSpace(data), []byte("OK")) {
				if _, err := writer.Write(data); err != nil {
					return err
				}
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("1:80:24:")); err != nil {
			return err
		}
	}

	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil || websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return err
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		if _, err := writer.Write(data); err != nil {
			return err
		}
	}
}

func (c *Client) consoleHandshake(ticket string) string {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return ""
	}
	tokenID := strings.TrimSpace(c.tokenID)
	if tokenID == "" {
		return ticket + "\n"
	}
	return tokenID + ":" + ticket + "\n"
}

func (c *Client) postTask(ctx context.Context, endpoint string, values url.Values) (string, error) {
	var raw json.RawMessage
	if err := c.postJSON(ctx, endpoint, values, &raw); err != nil {
		return "", err
	}
	return parseTaskID(raw), nil
}

func (c *Client) postJSON(ctx context.Context, endpoint string, values url.Values, out any) error {
	var body io.Reader
	contentType := ""
	if values != nil {
		encoded := values.Encode()
		body = strings.NewReader(encoded)
		contentType = "application/x-www-form-urlencoded"
	}
	return c.doJSON(ctx, http.MethodPost, endpoint, nil, contentType, body, out)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, query url.Values, out any) error {
	return c.doJSON(ctx, http.MethodGet, endpoint, query, "", nil, out)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, query url.Values, contentType string, body io.Reader, out any) error {
	u := c.apiURL(endpoint)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.addAuth(req.Header)

	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return httpStatusError{method: method, url: u.Redacted(), statusCode: res.StatusCode, status: res.Status, body: string(data)}
	}

	var envelope apiEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode Proxmox response from %s %s: %w", method, u.Redacted(), err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("Proxmox API errors: %v", envelope.Errors)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], envelope.Data...)
		return nil
	}
	if len(bytes.TrimSpace(envelope.Data)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode Proxmox data from %s %s: %w", method, u.Redacted(), err)
	}
	return nil
}

func (c *Client) apiURL(endpoint string) *url.URL {
	u := *c.baseURL
	fullPath := strings.TrimRight(c.baseURL.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	if strings.Contains(fullPath, "%") {
		if unescaped, err := url.PathUnescape(fullPath); err == nil {
			u.Path = unescaped
			u.RawPath = fullPath
			return &u
		}
	}
	u.Path = fullPath
	u.RawPath = ""
	return &u
}

func (c *Client) websocketURL(endpoint string) *url.URL {
	u := c.apiURL(endpoint)
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u
}

func (c *Client) addAuth(header http.Header) {
	header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", c.tokenID, c.tokenSecret))
}

func isJSONFalse(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return !v
	case float64:
		return v == 0
	case string:
		return v == "0" || strings.EqualFold(v, "false")
	default:
		return false
	}
}

func storageContentAllows(content, want string) bool {
	for _, part := range strings.Split(content, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

type flexibleInt64 struct {
	Value int64
	Set   bool
}

func (v *flexibleInt64) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("parse integer %q: %w", text, err)
		}
		v.Value = value
		v.Set = true
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return err
	}
	value, err := number.Int64()
	if err != nil {
		return fmt.Errorf("parse integer %q: %w", number.String(), err)
	}
	v.Value = value
	v.Set = true
	return nil
}

func parseJSONInt(raw json.RawMessage) (int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, errors.New("empty Proxmox integer response")
	}
	if text, ok := parseJSONString(raw); ok {
		value, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, fmt.Errorf("parse Proxmox integer %q: %w", text, err)
		}
		return value, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("parse Proxmox integer response %s: %w", raw, err)
	}
	return value, nil
}

func parseJSONString(raw json.RawMessage) (string, bool) {
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

func parseTaskID(raw json.RawMessage) string {
	if text, ok := parseJSONString(raw); ok {
		return strings.TrimSpace(text)
	}
	var data struct {
		UPID string `json:"upid"`
	}
	if err := json.Unmarshal(raw, &data); err == nil {
		return strings.TrimSpace(data.UPID)
	}
	return ""
}

type httpStatusError struct {
	method     string
	url        string
	statusCode int
	status     string
	body       string
}

func (e httpStatusError) Error() string {
	body := strings.TrimSpace(e.body)
	if body == "" {
		return fmt.Sprintf("%s %s: %s", e.method, e.url, e.status)
	}
	if len(body) > 500 {
		body = body[:500] + "..."
	}
	return fmt.Sprintf("%s %s: %s: %s", e.method, e.url, e.status, body)
}

func isNotFoundError(err error) bool {
	var statusErr httpStatusError
	return errors.As(err, &statusErr) && statusErr.statusCode == http.StatusNotFound
}
