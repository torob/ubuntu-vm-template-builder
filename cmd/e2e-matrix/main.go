package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "e2e.config.yaml"
	defaultISODir     = ".e2e-cache/isos"
	defaultWorkDir    = ".e2e-runs"
	defaultDiskSize   = "20G"
	defaultKeep       = "failures"

	bootModeUEFI = "uefi"
	bootModeBIOS = "bios"

	diskFormatRaw   = "raw"
	diskFormatQCOW2 = "qcow2"
	diskFormatVMDK  = "vmdk"

	guestUsername     = "e2euser"
	guestPassword     = "E2ePassword1!"
	guestPasswordHash = "$6$e2etest$d/P5M0cvmjePuTYQQiElR3ZZ5nK0az0Ru4MGeEhLLi7YkWHCzt06INR3iZtUr4REGc7Ix02w5bvm56Yb5BXcv."

	installTimeout = 90 * time.Minute
	bootTimeout    = 25 * time.Minute
	sshPollDelay   = 5 * time.Second
	qemuStopGrace  = 10 * time.Second
)

type matrixConfig struct {
	ISODir         string          `yaml:"iso_dir"`
	WorkDir        string          `yaml:"work_dir"`
	DiskSize       string          `yaml:"disk_size"`
	Concurrency    int             `yaml:"concurrency"`
	BootModes      []string        `yaml:"boot_modes"`
	DiskFormats    []string        `yaml:"disk_formats"`
	Keep           string          `yaml:"keep"`
	UbuntuVersions []ubuntuVersion `yaml:"ubuntu_versions"`
}

type ubuntuVersion struct {
	Name   string `yaml:"name"`
	ISOURL string `yaml:"iso_url"`
	SHA256 string `yaml:"sha256"`
}

type matrixCase struct {
	Index      int
	Version    ubuntuVersion
	ISOPath    string
	DiskFormat string
	BootMode   string
}

type caseResult struct {
	Case     matrixCase
	Dir      string
	Logs     []string
	Kept     bool
	Duration time.Duration
	Err      error
}

type matrixRunner struct {
	Config        matrixConfig
	InstallerPath string
	RunRoot       string
	Stdout        io.Writer
	LogMu         *sync.Mutex
}

type ovmfFirmware struct {
	CodePath string
	VarsPath string
}

var ovmfFirmwareCandidates = []ovmfFirmware{
	{CodePath: "/usr/share/OVMF/OVMF_CODE_4M.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	{CodePath: "/usr/share/OVMF/OVMF_CODE.fd", VarsPath: "/usr/share/OVMF/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE.fd", VarsPath: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/ovmf/OVMF_CODE_4M.fd", VarsPath: "/usr/share/edk2/ovmf/OVMF_VARS_4M.fd"},
	{CodePath: "/usr/share/edk2/x64/OVMF_CODE.fd", VarsPath: "/usr/share/edk2/x64/OVMF_VARS.fd"},
	{CodePath: "/usr/share/edk2/x64/OVMF_CODE.4m.fd", VarsPath: "/usr/share/edk2/x64/OVMF_VARS.4m.fd"},
	{CodePath: "/usr/share/qemu/ovmf-x86_64-code.bin", VarsPath: "/usr/share/qemu/ovmf-x86_64-vars.bin"},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("e2e-matrix", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", defaultConfigPath, "matrix config YAML path")
	concurrencyOverride := flags.Int("concurrency", 0, "override config concurrency")
	dryRun := flags.Bool("dry-run", false, "print expanded cases without downloading or running")
	keepOverride := flags.String("keep", "", "override artifact retention: all, failures, or none")
	flags.Usage = func() {
		printUsage(stderr, flags.Name())
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := loadMatrixConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if *concurrencyOverride < 0 {
		fmt.Fprintln(stderr, "Error: --concurrency must be greater than zero")
		return 1
	}
	if *concurrencyOverride > 0 {
		cfg.Concurrency = *concurrencyOverride
	}
	if strings.TrimSpace(*keepOverride) != "" {
		cfg.Keep = strings.TrimSpace(*keepOverride)
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	cases := cfg.expandCases()
	if *dryRun {
		printCases(stdout, cases)
		return 0
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	cfg.ISODir, err = filepath.Abs(cfg.ISODir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: resolve ISO cache directory: %v\n", err)
		return 1
	}
	cfg.WorkDir, err = filepath.Abs(cfg.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: resolve work directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(cfg.ISODir, 0o755); err != nil {
		fmt.Fprintf(stderr, "Error: create ISO cache directory: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "Error: create work directory: %v\n", err)
		return 1
	}
	if err := checkRunnerPrerequisites(cfg); err != nil {
		fmt.Fprintf(stderr, "Error: prerequisite check failed: %v\n", err)
		return 1
	}

	installerPath, err := buildInstaller(ctx, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	isoPaths, err := prepareISOs(ctx, stdout, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	for idx := range cases {
		cases[idx].ISOPath = isoPaths[cases[idx].Version.Name]
	}

	runRoot, err := createRunRoot(cfg.WorkDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: create run directory: %v\n", err)
		return 1
	}

	runner := matrixRunner{
		Config:        cfg,
		InstallerPath: installerPath,
		RunRoot:       runRoot,
		Stdout:        stdout,
		LogMu:         &sync.Mutex{},
	}

	fmt.Fprintf(stdout, "Running %d cases with concurrency %d\n", len(cases), cfg.Concurrency)
	fmt.Fprintf(stdout, "Run directory: %s\n", runRoot)

	results := runner.runCases(ctx, cases)
	exitCode := printSummary(stdout, results)
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "Interrupted; running child processes were stopped.")
		return 130
	}
	return exitCode
}

func printUsage(out io.Writer, program string) {
	fmt.Fprintf(out, "Usage: %s --config e2e.config.yaml [--concurrency n] [--dry-run] [--keep all|failures|none]\n\n", program)
	fmt.Fprintln(out, "The config expands ubuntu_versions x disk_formats x boot_modes.")
	fmt.Fprintln(out, "Each case installs an image, boots it with QEMU, and verifies SSH, hostname, password, and boot-mode-specific storage.")
	fmt.Fprintf(out, "Default config path: %s\n", defaultConfigPath)
	fmt.Fprintln(out, "See e2e.config.yaml for the config shape.")
}

func createRunRoot(workDir string) (string, error) {
	base := time.Now().UTC().Format("20060102T150405Z")
	for attempt := 0; attempt < 100; attempt++ {
		name := base
		if attempt > 0 {
			name = fmt.Sprintf("%s-%02d", base, attempt+1)
		}
		runRoot := filepath.Join(workDir, name)
		err := os.Mkdir(runRoot, 0o755)
		if err == nil {
			return runRoot, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("could not allocate a unique run directory under %s", workDir)
}

func checkRunnerPrerequisites(cfg matrixConfig) error {
	requiredCommands := []string{"qemu-system-x86_64", "ssh", "ssh-keygen"}
	if hasDiskFormat(cfg, diskFormatQCOW2) || hasDiskFormat(cfg, diskFormatVMDK) {
		requiredCommands = append(requiredCommands, "qemu-img")
	}
	for _, command := range requiredCommands {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("missing required command %q", command)
		}
	}
	if hasBootMode(cfg, bootModeUEFI) {
		if _, err := findOVMFFirmware(); err != nil {
			return err
		}
	}
	if err := checkKVMAccess(); err != nil {
		return err
	}
	return nil
}

func hasDiskFormat(cfg matrixConfig, diskFormat string) bool {
	for _, candidate := range cfg.DiskFormats {
		if candidate == diskFormat {
			return true
		}
	}
	return false
}

func hasBootMode(cfg matrixConfig, bootMode string) bool {
	for _, candidate := range cfg.BootModes {
		if candidate == bootMode {
			return true
		}
	}
	return false
}

func checkKVMAccess() error {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("/dev/kvm not found; enable virtualization and load the KVM module")
		}
		return fmt.Errorf("check /dev/kvm: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("/dev/kvm is a directory, expected a device")
	}
	file, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("/dev/kvm is not accessible for read/write: %w", err)
	}
	return file.Close()
}

func loadMatrixConfig(configPath string) (matrixConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && configPath == defaultConfigPath {
			return matrixConfig{}, fmt.Errorf("config file %q not found; create e2e.config.yaml or pass --config", configPath)
		}
		return matrixConfig{}, fmt.Errorf("read config file %q: %w", configPath, err)
	}

	var cfg matrixConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return matrixConfig{}, fmt.Errorf("parse config file %q: %w", configPath, err)
	}
	return cfg, nil
}

func (c *matrixConfig) normalizeAndValidate() error {
	c.ISODir = strings.TrimSpace(c.ISODir)
	if c.ISODir == "" {
		c.ISODir = defaultISODir
	}
	c.WorkDir = strings.TrimSpace(c.WorkDir)
	if c.WorkDir == "" {
		c.WorkDir = defaultWorkDir
	}
	c.DiskSize = strings.TrimSpace(c.DiskSize)
	if c.DiskSize == "" {
		c.DiskSize = defaultDiskSize
	}
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.Concurrency < 0 {
		return fmt.Errorf("concurrency must be greater than zero")
	}
	if c.Keep = strings.ToLower(strings.TrimSpace(c.Keep)); c.Keep == "" {
		c.Keep = defaultKeep
	}
	if !validKeepPolicy(c.Keep) {
		return fmt.Errorf("unsupported keep policy %q; supported values: all, failures, none", c.Keep)
	}

	c.DiskFormats = normalizeList(c.DiskFormats)
	if len(c.DiskFormats) == 0 {
		c.DiskFormats = []string{diskFormatRaw, diskFormatQCOW2, diskFormatVMDK}
	}
	for _, format := range c.DiskFormats {
		if !validDiskFormat(format) {
			return fmt.Errorf("unsupported disk format %q; supported values: raw, qcow2, vmdk", format)
		}
	}

	c.BootModes = normalizeList(c.BootModes)
	if len(c.BootModes) == 0 {
		c.BootModes = []string{bootModeUEFI, bootModeBIOS}
	}
	for _, mode := range c.BootModes {
		if !validBootMode(mode) {
			return fmt.Errorf("unsupported boot mode %q; supported values: uefi, bios", mode)
		}
	}

	if len(c.UbuntuVersions) == 0 {
		return fmt.Errorf("ubuntu_versions must contain at least one version")
	}
	seenVersions := make(map[string]bool)
	for idx := range c.UbuntuVersions {
		version := &c.UbuntuVersions[idx]
		version.Name = strings.TrimSpace(version.Name)
		version.ISOURL = strings.TrimSpace(version.ISOURL)
		version.SHA256 = normalizeSHA256(version.SHA256)
		if version.Name == "" {
			return fmt.Errorf("ubuntu_versions[%d].name is required", idx)
		}
		if seenVersions[version.Name] {
			return fmt.Errorf("ubuntu_versions contains duplicate name %q", version.Name)
		}
		seenVersions[version.Name] = true
		if version.ISOURL == "" {
			return fmt.Errorf("ubuntu_versions[%d].iso_url is required", idx)
		}
		if err := validateHTTPURL(version.ISOURL); err != nil {
			return fmt.Errorf("ubuntu_versions[%d].iso_url: %w", idx, err)
		}
		if version.SHA256 != "" && !validSHA256(version.SHA256) {
			return fmt.Errorf("ubuntu_versions[%d].sha256 must be a 64-character hex SHA256 digest", idx)
		}
	}

	return nil
}

func normalizeList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func validDiskFormat(format string) bool {
	switch format {
	case diskFormatRaw, diskFormatQCOW2, diskFormatVMDK:
		return true
	default:
		return false
	}
}

func validBootMode(mode string) bool {
	switch mode {
	case bootModeUEFI, bootModeBIOS:
		return true
	default:
		return false
	}
}

func validKeepPolicy(policy string) bool {
	switch policy {
	case "all", "failures", "none":
		return true
	default:
		return false
	}
}

func validateHTTPURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must use http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}
	return nil
}

func normalizeSHA256(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), ""))
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (c matrixConfig) expandCases() []matrixCase {
	var cases []matrixCase
	for _, version := range c.UbuntuVersions {
		for _, format := range c.DiskFormats {
			for _, mode := range c.BootModes {
				cases = append(cases, matrixCase{
					Index:      len(cases) + 1,
					Version:    version,
					DiskFormat: format,
					BootMode:   mode,
				})
			}
		}
	}
	return cases
}

func printCases(out io.Writer, cases []matrixCase) {
	fmt.Fprintf(out, "Expanded %d e2e cases:\n", len(cases))
	for _, testCase := range cases {
		fmt.Fprintf(out, "%2d. %s\n", testCase.Index, testCase.ID())
		fmt.Fprintf(out, "    ISO: %s\n", testCase.Version.ISOURL)
	}
}

func buildInstaller(ctx context.Context, stdout, stderr io.Writer) (string, error) {
	installerPath, err := filepath.Abs("ubuntu-vm-template-builder")
	if err != nil {
		return "", err
	}

	goPath, err := findGo()
	if err != nil {
		return "", err
	}

	fmt.Fprintf(stdout, "Building installer with %s\n", goPath)
	cmd := exec.CommandContext(ctx, goPath, "build", "-o", installerPath, ".")
	cmd.Env = goEnv()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build ubuntu-vm-template-builder: %w", err)
	}
	return installerPath, nil
}

func findGo() (string, error) {
	var candidates []string
	home, _ := os.UserHomeDir()
	if home != "" {
		versioned, _ := filepath.Glob(filepath.Join(home, ".tools", "go", "*", "bin", "go"))
		sort.Slice(versioned, func(i, j int) bool {
			return compareGoVersionPath(versioned[i], versioned[j]) > 0
		})
		candidates = append(candidates, versioned...)
		candidates = append(candidates, filepath.Join(home, ".tools", "bin", "go"))
	}
	if systemGo, err := exec.LookPath("go"); err == nil {
		candidates = append(candidates, systemGo)
	}

	seen := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		if !isExecutable(candidate) {
			continue
		}
		if goCommandAtLeast(candidate, 1, 26) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Go 1.26 or newer not found under $HOME/.tools or PATH")
}

func compareGoVersionPath(a, b string) int {
	av := parseVersion(filepath.Base(filepath.Dir(filepath.Dir(a))))
	bv := parseVersion(filepath.Base(filepath.Dir(filepath.Dir(b))))
	for idx := 0; idx < len(av) || idx < len(bv); idx++ {
		var ai, bi int
		if idx < len(av) {
			ai = av[idx]
		}
		if idx < len(bv) {
			bi = bv[idx]
		}
		if ai > bi {
			return 1
		}
		if ai < bi {
			return -1
		}
	}
	return strings.Compare(a, b)
}

func parseVersion(version string) []int {
	parts := strings.Split(version, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		value, _ := strconv.Atoi(part)
		out = append(out, value)
	}
	return out
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func goCommandAtLeast(goPath string, wantMajor, wantMinor int) bool {
	out, err := exec.Command(goPath, "version").Output()
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`go([0-9]+)\.([0-9]+)`)
	match := re.FindStringSubmatch(string(out))
	if match == nil {
		return false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return false
	}
	if major != wantMajor {
		return major > wantMajor
	}
	return minor >= wantMinor
}

func goEnv() []string {
	env := os.Environ()
	home, _ := os.UserHomeDir()
	if home == "" {
		return env
	}

	_ = os.MkdirAll(filepath.Join(home, ".cache", "go-build"), 0o755)
	_ = os.MkdirAll(filepath.Join(home, "go"), 0o755)

	pathValue := filepath.Join(home, ".tools", "bin") + string(os.PathListSeparator) + os.Getenv("PATH")
	env = setEnv(env, "PATH", pathValue)
	env = setEnv(env, "GOCACHE", filepath.Join(home, ".cache", "go-build"))
	env = setEnv(env, "GOPATH", filepath.Join(home, "go"))
	env = setEnv(env, "GOMODCACHE", filepath.Join(home, "go", "pkg", "mod"))
	env = setEnv(env, "GOPROXY", "https://proxy.golang.org,direct")
	return env
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for idx := range env {
		if strings.HasPrefix(env[idx], prefix) {
			env[idx] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func prepareISOs(ctx context.Context, stdout io.Writer, cfg matrixConfig) (map[string]string, error) {
	paths := make(map[string]string)
	for _, version := range cfg.UbuntuVersions {
		isoPath := filepath.Join(cfg.ISODir, isoFileName(version))
		if _, err := os.Stat(isoPath); err == nil {
			fmt.Fprintf(stdout, "Using cached ISO for Ubuntu %s: %s\n", version.Name, isoPath)
		} else if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stdout, "Downloading Ubuntu %s ISO: %s\n", version.Name, version.ISOURL)
			if err := downloadFile(ctx, version.ISOURL, isoPath); err != nil {
				return nil, fmt.Errorf("download Ubuntu %s ISO: %w", version.Name, err)
			}
		} else {
			return nil, fmt.Errorf("check ISO cache %q: %w", isoPath, err)
		}

		if version.SHA256 == "" {
			fmt.Fprintf(stdout, "Warning: no SHA256 configured for Ubuntu %s; ISO integrity was not verified\n", version.Name)
		} else if err := verifySHA256(isoPath, version.SHA256); err != nil {
			return nil, fmt.Errorf("verify Ubuntu %s ISO: %w", version.Name, err)
		}
		paths[version.Name] = isoPath
	}
	return paths, nil
}

func isoFileName(version ubuntuVersion) string {
	parsed, err := url.Parse(version.ISOURL)
	if err == nil {
		base := path.Base(parsed.Path)
		if base != "." && base != "/" && strings.TrimSpace(base) != "" {
			return base
		}
	}
	return safeName(version.Name) + ".iso"
}

func downloadFile(ctx context.Context, rawURL, destinationPath string) error {
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o755); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}

	tempPath := destinationPath + ".part"
	out, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, response.Body); err != nil {
		out.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return os.Rename(tempPath, destinationPath)
}

func verifySHA256(filePath, want string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", filePath, got, want)
	}
	return nil
}

func (r matrixRunner) runCases(ctx context.Context, cases []matrixCase) []caseResult {
	jobs := make(chan matrixCase)
	results := make(chan caseResult)
	workerCount := r.Config.Concurrency
	if workerCount > len(cases) {
		workerCount = len(cases)
	}

	var wg sync.WaitGroup
	for idx := 0; idx < workerCount; idx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for testCase := range jobs {
				results <- r.runCase(ctx, testCase)
			}
		}()
	}

	go func() {
	send:
		for _, testCase := range cases {
			select {
			case <-ctx.Done():
				break send
			case jobs <- testCase:
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	collected := make([]caseResult, 0, len(cases))
	for result := range results {
		collected = append(collected, result)
	}
	sort.Slice(collected, func(i, j int) bool {
		return collected[i].Case.Index < collected[j].Case.Index
	})
	return collected
}

func (r matrixRunner) runCase(parent context.Context, testCase matrixCase) caseResult {
	start := time.Now()
	caseDir := filepath.Join(r.RunRoot, testCase.ID())
	result := caseResult{
		Case: testCase,
		Dir:  caseDir,
		Logs: []string{
			filepath.Join(caseDir, "install.log"),
			filepath.Join(caseDir, "boot.log"),
		},
	}

	r.printf("[%s] start\n", testCase.ID())
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		result.Err = fmt.Errorf("create case directory: %w", err)
		return result.withDuration(start)
	}

	keyPath := filepath.Join(caseDir, "id_ed25519")
	publicKey, err := generateKeypair(parent, keyPath)
	if err != nil {
		result.Err = fmt.Errorf("generate SSH keypair: %w", err)
		return r.finishCase(result.withDuration(start))
	}

	hostname := testCase.Hostname()
	userData := generateUserData(testCase.BootMode, hostname, publicKey)
	userDataPath := filepath.Join(caseDir, "autoinstall.yaml")
	if err := os.WriteFile(userDataPath, []byte(userData), 0o644); err != nil {
		result.Err = fmt.Errorf("write user-data: %w", err)
		return r.finishCase(result.withDuration(start))
	}

	imagePath := filepath.Join(caseDir, "image."+testCase.DiskFormat)
	installCtx, cancelInstall := context.WithTimeout(parent, installTimeout)
	installArgs := []string{
		"--iso", testCase.ISOPath,
		"--image", imagePath,
		"--disk-size", r.Config.DiskSize,
		"--user-data", userDataPath,
		"--disk-format", testCase.DiskFormat,
		"--boot-mode", testCase.BootMode,
	}
	r.printf("[%s] installing image; log: %s\n", testCase.ID(), filepath.Join(caseDir, "install.log"))
	err = runLoggedCommand(installCtx, caseDir, filepath.Join(caseDir, "install.log"), r.InstallerPath, installArgs...)
	cancelInstall()
	if err != nil {
		result.Err = fmt.Errorf("install image: %w", err)
		return r.finishCase(result.withDuration(start))
	}

	bootCtx, cancelBoot := context.WithTimeout(parent, bootTimeout)
	r.printf("[%s] booting image and waiting for SSH; log: %s\n", testCase.ID(), filepath.Join(caseDir, "boot.log"))
	err = r.verifyBoot(bootCtx, caseDir, imagePath, keyPath, publicKey, hostname, testCase)
	cancelBoot()
	if err != nil {
		result.Err = fmt.Errorf("boot verification: %w", err)
		return r.finishCase(result.withDuration(start))
	}

	return r.finishCase(result.withDuration(start))
}

func (r matrixRunner) finishCase(result caseResult) caseResult {
	if result.Err == nil {
		r.printf("[%s] pass in %s\n", result.Case.ID(), result.Duration.Round(time.Second))
	} else {
		r.printf("[%s] fail in %s: %v\n", result.Case.ID(), result.Duration.Round(time.Second), result.Err)
	}

	keep := r.Config.Keep
	result.Kept = !(keep == "none" || keep == "failures" && result.Err == nil)
	if !result.Kept {
		if err := os.RemoveAll(result.Dir); err != nil {
			r.printf("[%s] warning: cleanup failed: %v\n", result.Case.ID(), err)
		}
	}
	return result
}

func (r matrixRunner) printf(format string, args ...any) {
	r.LogMu.Lock()
	defer r.LogMu.Unlock()
	fmt.Fprintf(r.Stdout, format, args...)
}

func (result caseResult) withDuration(start time.Time) caseResult {
	result.Duration = time.Since(start)
	return result
}

func generateKeypair(ctx context.Context, keyPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath, "-C", "ubuntu-vm-template-builder-e2e")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(publicKey)), nil
}

func runLoggedCommand(ctx context.Context, dir, logPath, name string, args ...string) error {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureCommandProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	err = waitCommand(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%s %s failed: %w; see %s", name, strings.Join(args, " "), err, logPath)
	}
	return nil
}

func (r matrixRunner) verifyBoot(ctx context.Context, caseDir, imagePath, keyPath, publicKey, hostname string, testCase matrixCase) error {
	port, err := tcpPortForCase(testCase.Index)
	if err != nil {
		return err
	}

	logPath := filepath.Join(caseDir, "boot.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	args, err := bootQEMUArgs(caseDir, imagePath, testCase.DiskFormat, testCase.BootMode, port)
	if err != nil {
		return err
	}
	cmd := exec.Command("qemu-system-x86_64", args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureCommandProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	defer stopCommand(cmd, done)

	if err := waitForSSH(ctx, keyPath, port); err != nil {
		return fmt.Errorf("%w; see %s", err, logPath)
	}

	checks := []struct {
		name    string
		command string
	}{
		{name: "hostname", command: "test \"$(hostname)\" = " + shellQuote(hostname)},
		{name: "sudo password", command: "printf '%s\\n' " + shellQuote(guestPassword) + " | sudo -S -p '' true"},
		{name: "authorized key", command: "grep -qxF " + shellQuote(publicKey) + " ~/.ssh/authorized_keys"},
	}
	if testCase.BootMode == bootModeUEFI {
		checks = append(checks,
			struct {
				name    string
				command string
			}{name: "UEFI runtime", command: "test -d /sys/firmware/efi"},
			struct {
				name    string
				command string
			}{name: "ESP mounted", command: "findmnt /boot/efi >/dev/null"},
			struct {
				name    string
				command string
			}{name: "fallback bootloader", command: "test -e /boot/efi/EFI/BOOT/BOOTX64.EFI"},
			struct {
				name    string
				command string
			}{name: "ESP partition type", command: "esp=$(findmnt -nro SOURCE /boot/efi) && test \"$(lsblk -ndo PARTTYPE \"$esp\")\" = c12a7328-f81f-11d2-ba4b-00a0c93ec93b"},
		)
	} else {
		checks = append(checks,
			struct {
				name    string
				command string
			}{name: "BIOS runtime", command: "test ! -d /sys/firmware/efi"},
			struct {
				name    string
				command string
			}{name: "BIOS boot partition", command: "lsblk -rno PARTTYPE | grep -qi '^21686148-6449-6e6f-744e-656564454649$'"},
		)
	}

	for _, check := range checks {
		if err := runSSH(ctx, keyPath, port, check.command); err != nil {
			return fmt.Errorf("%s check failed: %w", check.name, err)
		}
	}

	return nil
}

func bootQEMUArgs(caseDir, imagePath, diskFormat, bootMode string, sshPort int) ([]string, error) {
	args := []string{
		"--enable-kvm",
		"-no-reboot",
		"-m", "2048",
		"-nographic",
		"-boot", "c",
		"-device", "virtio-rng-pci",
	}

	if bootMode == bootModeUEFI {
		firmware, err := findOVMFFirmware()
		if err != nil {
			return nil, err
		}
		varsPath := filepath.Join(caseDir, "OVMF_VARS.fd")
		if err := copyFile(firmware.VarsPath, varsPath); err != nil {
			return nil, fmt.Errorf("prepare OVMF variables store: %w", err)
		}
		args = append(args,
			"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", firmware.CodePath),
			"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", varsPath),
		)
	}

	args = append(args,
		"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio", imagePath, diskFormat),
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:127.0.0.1:%d-:22", sshPort),
		"-device", "virtio-net-pci,netdev=net0",
	)
	return args, nil
}

func findOVMFFirmware() (ovmfFirmware, error) {
	for _, candidate := range ovmfFirmwareCandidates {
		if regularFile(candidate.CodePath) && regularFile(candidate.VarsPath) {
			return candidate, nil
		}
	}
	return ovmfFirmware{}, fmt.Errorf("missing OVMF UEFI firmware; install OVMF or test only boot_modes: [bios]")
}

func regularFile(filePath string) bool {
	info, err := os.Stat(filePath)
	return err == nil && info.Mode().IsRegular()
}

func copyFile(sourcePath, destinationPath string) error {
	in, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func waitForSSH(ctx context.Context, keyPath string, port int) error {
	var lastErr error
	ticker := time.NewTicker(sshPollDelay)
	defer ticker.Stop()

	for {
		lastErr = runSSH(ctx, keyPath, port, "true")
		if lastErr == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("SSH did not become ready before timeout: last error: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func runSSH(ctx context.Context, keyPath string, port int, remoteCommand string) error {
	args := []string{
		"-i", keyPath,
		"-p", strconv.Itoa(port),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("%s@127.0.0.1", guestUsername),
		remoteCommand,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func tcpPortForCase(caseIndex int) (int, error) {
	if caseIndex < 1 {
		caseIndex = 1
	}
	start := 22000 + caseIndex
	for port := start; port < 65000; port += 1000 {
		if tcpPortAvailable(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available local TCP port found for case %d", caseIndex)
}

func tcpPortAvailable(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	defer listener.Close()
	return true
}

func configureCommandProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func waitCommand(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = signalCommandProcessGroup(cmd, syscall.SIGTERM)
		timer := time.NewTimer(qemuStopGrace)
		defer timer.Stop()
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("%w after timeout: %v", ctx.Err(), err)
			}
			return ctx.Err()
		case <-timer.C:
			_ = signalCommandProcessGroup(cmd, syscall.SIGKILL)
			return fmt.Errorf("%w; command killed", ctx.Err())
		}
	}
}

func stopCommand(cmd *exec.Cmd, done <-chan error) {
	_ = signalCommandProcessGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(qemuStopGrace)
	defer timer.Stop()

	select {
	case <-done:
		return
	case <-timer.C:
		_ = signalCommandProcessGroup(cmd, syscall.SIGKILL)
		<-done
	}
}

func signalCommandProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func printSummary(out io.Writer, results []caseResult) int {
	passed := 0
	for _, result := range results {
		if result.Err == nil {
			passed++
		}
	}
	failed := len(results) - passed

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Summary: %d passed, %d failed, %d total\n", passed, failed, len(results))
	for _, result := range results {
		status := "PASS"
		if result.Err != nil {
			status = "FAIL"
		}
		fmt.Fprintf(out, "[%s] %s (%s)\n", status, result.Case.ID(), result.Duration.Round(time.Second))
		if result.Err != nil {
			fmt.Fprintf(out, "  Error: %v\n", result.Err)
			if result.Kept {
				fmt.Fprintf(out, "  Artifacts: %s\n", result.Dir)
				for _, logPath := range result.Logs {
					fmt.Fprintf(out, "  Log: %s\n", logPath)
				}
			} else {
				fmt.Fprintln(out, "  Artifacts removed by keep policy")
			}
		}
	}

	if failed > 0 {
		return 1
	}
	return 0
}

func (c matrixCase) ID() string {
	return safeName(fmt.Sprintf("ubuntu-%s-%s-%s", c.Version.Name, c.DiskFormat, c.BootMode))
}

func (c matrixCase) Hostname() string {
	return safeHostname(fmt.Sprintf("e2e-%s-%s-%s", c.Version.Name, c.DiskFormat, c.BootMode))
}

func safeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}

	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}

	clean := strings.Trim(b.String(), "-_.")
	if clean == "" {
		return "unnamed"
	}
	return clean
}

func safeHostname(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	clean := strings.Trim(b.String(), "-")
	if clean == "" {
		clean = "e2e"
	}
	if len(clean) > 63 {
		clean = strings.Trim(clean[:63], "-")
	}
	return clean
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func generateUserData(bootMode, hostname, publicKey string) string {
	return fmt.Sprintf(`#cloud-config
autoinstall:
  version: 1
  identity:
    hostname: %s
    username: %s
    password: "%s"
  refresh-installer:
    update: false
  source:
    search_drivers: false
    id: ubuntu-server
  network:
    version: 2
    ethernets:
      all:
        match:
          name: "e*"
        dhcp4: true
  apt:
    geoip: false
  ssh:
    install-server: true
    authorized-keys:
      - %s
    allow-pw: true
  timezone: "Asia/Tehran"
  locale: en_US.UTF-8
  keyboard:
    layout: us
%s`, hostname, guestUsername, guestPasswordHash, publicKey, storageConfig(bootMode))
}

func storageConfig(bootMode string) string {
	if bootMode == bootModeUEFI {
		return `  storage:
    config:
      - id: disk0
        type: disk
        match:
          size: largest
        ptable: gpt
        wipe: superblock-recursive
        preserve: false
      - id: part-efi
        type: partition
        device: disk0
        size: 512M
        flag: boot
        grub_device: true
      - id: part-boot
        type: partition
        device: disk0
        size: 1G
      - id: part-lvm
        type: partition
        device: disk0
        size: -1
      - id: vg-os
        type: lvm_volgroup
        name: os
        devices: [part-lvm]
      - id: lv-root
        type: lvm_partition
        name: root
        volgroup: vg-os
        size: 7G
      - id: lv-var
        type: lvm_partition
        name: var
        volgroup: vg-os
        size: -1
      - id: fs-boot
        type: format
        volume: part-boot
        fstype: ext4
      - id: fs-efi
        type: format
        volume: part-efi
        fstype: fat32
      - id: fs-root
        type: format
        volume: lv-root
        fstype: ext4
      - id: fs-var
        type: format
        volume: lv-var
        fstype: ext4
      - id: mount-boot
        type: mount
        device: fs-boot
        path: /boot
      - id: mount-efi
        type: mount
        device: fs-efi
        path: /boot/efi
      - id: mount-root
        type: mount
        device: fs-root
        path: /
      - id: mount-var
        type: mount
        device: fs-var
        path: /var
  late-commands:
    - curtin in-target --target=/target -- grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=ubuntu --removable --recheck
`
	}

	return `  storage:
    config:
      - id: disk0
        type: disk
        match:
          size: largest
        ptable: gpt
        wipe: superblock-recursive
        preserve: false
        grub_device: true
      - id: part-bios
        type: partition
        device: disk0
        size: 1M
        flag: bios_grub
      - id: part-boot
        type: partition
        device: disk0
        size: 1G
        flag: boot
      - id: part-lvm
        type: partition
        device: disk0
        size: -1
      - id: vg-os
        type: lvm_volgroup
        name: os
        devices: [part-lvm]
      - id: lv-root
        type: lvm_partition
        name: root
        volgroup: vg-os
        size: 7G
      - id: lv-var
        type: lvm_partition
        name: var
        volgroup: vg-os
        size: -1
      - id: fs-boot
        type: format
        volume: part-boot
        fstype: ext4
      - id: fs-root
        type: format
        volume: lv-root
        fstype: ext4
      - id: fs-var
        type: format
        volume: lv-var
        fstype: ext4
      - id: mount-boot
        type: mount
        device: fs-boot
        path: /boot
      - id: mount-root
        type: mount
        device: fs-root
        path: /
      - id: mount-var
        type: mount
        device: fs-var
        path: /var
`
}
