package offlineapt

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
)

const (
	ISORepoPath            = "/ubuntu-vm-template-builder/offline-apt"
	ISOInstallConfigPath   = "/ubuntu-vm-template-builder/offline-apt-install"
	ISOInstallScriptPath   = "/ubuntu-vm-template-builder/scripts/install-offline-packages.sh"
	guestRepoPath          = "/var/lib/ubuntu-vm-template-builder/offline-apt"
	targetRepoPath         = "/target" + guestRepoPath
	targetSourceList       = "/etc/apt/sources.list.d/ubuntu-vm-template-builder-offline.list"
	targetSourceParts      = "/tmp/ubuntu-vm-template-builder-empty-sources.d"
	ubuntuArchiveKeyring   = "/usr/share/keyrings/ubuntu-archive-keyring.gpg"
	aptArchitecture        = "amd64"
	aptAllArchitecture     = "all"
	offlineSuite           = "offline"
	offlineComponent       = "main"
	repositoryWorkDirName  = "offline-apt-repo"
	aptWorkDirName         = "offline-apt-work"
	downloadWorkDirName    = "offline-apt-downloads"
	dep11IndexTargetOption = "Acquire::IndexTargets::deb::DEP-11::DefaultEnabled=false"
	cnfIndexTargetOption   = "Acquire::IndexTargets::deb::CNF::DefaultEnabled=false"
)

var (
	defaultSuites     = []string{"release", "updates", "security"}
	defaultComponents = []string{"main", "restricted", "universe", "multiverse"}
)

type Config struct {
	APTURL     string   `yaml:"apt_url"`
	Packages   []string `yaml:"packages"`
	Components []string `yaml:"components"`
	Suites     []string `yaml:"suites"`
}

type Repository struct {
	Path         string
	Packages     []string
	Sources      []RepositorySource
	PackageFiles []string
}

type RepositorySource struct {
	Suite      string
	Components []string
}

type InstallConfig struct {
	Packages []string
	Sources  []RepositorySource
}

type CommandRunner interface {
	Output(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

type packageURI struct {
	URI          string
	DownloadName string
	RelativePath string
}

type packageIndexRecord struct {
	RelativePath string
	Architecture string
	Text         string
}

func (ExecRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s failed: %w (%s)", commandString(name, args), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func LoadConfig(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read install extra packages config %q: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, fmt.Errorf("install extra packages config %q is empty", path)
	}

	cfg := Config{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse install extra packages config %q: %w", path, err)
	}
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid install extra packages config %q: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Enabled() bool {
	return strings.TrimSpace(c.APTURL) != "" || len(c.Packages) > 0 || len(c.Components) > 0 || len(c.Suites) > 0
}

func (c Config) Normalize() Config {
	c.APTURL = strings.TrimRight(strings.TrimSpace(c.APTURL), "/")
	c.Packages = normalizeList(c.Packages)
	c.Components = normalizeList(c.Components)
	c.Suites = normalizeList(c.Suites)
	if !c.Enabled() {
		return c
	}
	if len(c.Components) == 0 {
		c.Components = append([]string(nil), defaultComponents...)
	}
	if len(c.Suites) == 0 {
		c.Suites = append([]string(nil), defaultSuites...)
	}
	return c
}

func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if strings.TrimSpace(c.APTURL) == "" {
		return errors.New("apt_url is required")
	}
	if len(c.Packages) == 0 {
		return errors.New("packages must contain at least one package")
	}
	if _, err := url.ParseRequestURI(c.APTURL); err != nil {
		return fmt.Errorf("apt_url must be an absolute URL: %w", err)
	}
	for _, pkg := range c.Packages {
		if strings.TrimSpace(pkg) == "" {
			return errors.New("packages must not contain empty entries")
		}
		if strings.ContainsAny(pkg, " \t\r\n") {
			return fmt.Errorf("package %q must not contain whitespace", pkg)
		}
	}
	for _, component := range c.Components {
		if err := validateAPTName("component", component); err != nil {
			return err
		}
	}
	for _, suite := range c.Suites {
		if err := validateAPTName("suite", suite); err != nil {
			return err
		}
	}
	return nil
}

func CheckPrerequisites(c Config) error {
	c = c.Normalize()
	if !c.Enabled() {
		return nil
	}
	if err := c.Validate(); err != nil {
		return err
	}
	for _, command := range []string{"apt-get", "xorriso"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("missing required dependency for --install-extra-packages: %s", command)
		}
	}
	if info, err := os.Stat(ubuntuArchiveKeyring); err != nil {
		return fmt.Errorf("missing Ubuntu archive keyring for --install-extra-packages: %s", ubuntuArchiveKeyring)
	} else if info.IsDir() {
		return fmt.Errorf("Ubuntu archive keyring path is a directory, expected file: %s", ubuntuArchiveKeyring)
	}
	return nil
}

func BuildRepository(ctx context.Context, cfg Config, ubuntuISO, workDir string) (Repository, error) {
	return BuildRepositoryWithRunner(ctx, cfg, ubuntuISO, workDir, ExecRunner{})
}

func BuildRepositoryWithRunner(ctx context.Context, cfg Config, ubuntuISO, workDir string, runner CommandRunner) (Repository, error) {
	cfg = cfg.Normalize()
	if !cfg.Enabled() {
		return Repository{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return Repository{}, err
	}
	if runner == nil {
		runner = ExecRunner{}
	}

	codename, err := DetectISOCodename(ubuntuISO)
	if err != nil {
		return Repository{}, err
	}

	return buildRepositoryWithCodename(ctx, cfg, codename, workDir, runner)
}

func buildRepositoryWithCodename(ctx context.Context, cfg Config, codename, workDir string, runner CommandRunner) (Repository, error) {
	cfg = cfg.Normalize()
	if !cfg.Enabled() {
		return Repository{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return Repository{}, err
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if strings.TrimSpace(codename) == "" {
		return Repository{}, errors.New("Ubuntu codename is required")
	}

	suites := expandSuites(cfg.Suites, codename)
	repoDir := filepath.Join(workDir, repositoryWorkDirName)
	aptRoot := filepath.Join(workDir, aptWorkDirName)
	downloadDir := filepath.Join(workDir, downloadWorkDirName)
	if err := prepareAPTDirectories(downloadDir, aptRoot); err != nil {
		return Repository{}, err
	}
	if err := writeAPTSources(filepath.Join(aptRoot, "etc/apt/sources.list"), cfg, suites); err != nil {
		return Repository{}, err
	}

	aptOptions := isolatedAPTOptions(aptRoot, downloadDir)
	updateOutput, err := runner.Output(ctx, "", "apt-get", append(aptOptions, "update")...)
	if err != nil {
		return Repository{}, err
	}
	if err := checkAPTUpdateOutput(updateOutput); err != nil {
		return Repository{}, err
	}

	uriArgs := append(append([]string{}, aptOptions...), "-y", "--print-uris", "install")
	uriArgs = append(uriArgs, cfg.Packages...)
	uriOutput, err := runner.Output(ctx, "", "apt-get", uriArgs...)
	if err != nil {
		return Repository{}, err
	}
	packageURIs, err := parsePackageURIs(uriOutput)
	if err != nil {
		return Repository{}, err
	}
	if len(packageURIs) == 0 {
		return Repository{}, errors.New("apt-get produced no package download URIs")
	}

	installArgs := append(append([]string{}, aptOptions...), "-y", "--download-only", "install")
	installArgs = append(installArgs, cfg.Packages...)
	if _, err := runner.Output(ctx, "", "apt-get", installArgs...); err != nil {
		return Repository{}, err
	}

	if err := copySelectedPackages(downloadDir, repoDir, packageURIs); err != nil {
		return Repository{}, err
	}

	if err := writeTrimmedRepositoryMetadata(aptRoot, repoDir, packageURIs); err != nil {
		return Repository{}, err
	}

	repo := Repository{
		Path:         repoDir,
		Packages:     append([]string(nil), cfg.Packages...),
		Sources:      offlineRepositorySources(),
		PackageFiles: packageRelativePaths(packageURIs),
	}
	if err := ValidateRepository(repo); err != nil {
		return Repository{}, err
	}
	return repo, nil
}

func (r Repository) InstallConfig() InstallConfig {
	return InstallConfig{
		Packages: append([]string(nil), r.Packages...),
		Sources:  copySources(r.Sources),
	}
}

func (c InstallConfig) Enabled() bool {
	return len(c.Packages) > 0
}

func (c InstallConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if len(c.Sources) == 0 {
		return errors.New("offline package install sources must not be empty")
	}
	for _, pkg := range c.Packages {
		if strings.TrimSpace(pkg) == "" || strings.ContainsAny(pkg, " \t\r\n") {
			return fmt.Errorf("invalid offline package name %q", pkg)
		}
	}
	for _, source := range c.Sources {
		if err := validateAPTName("suite", source.Suite); err != nil {
			return err
		}
		if len(source.Components) == 0 {
			return fmt.Errorf("offline package install source %q has no components", source.Suite)
		}
		for _, component := range source.Components {
			if err := validateAPTName("component", component); err != nil {
				return err
			}
		}
	}
	return nil
}

func DetectISOCodename(ubuntuISO string) (string, error) {
	fs, closeFn, err := common.OpenISOFilesystem(ubuntuISO)
	if err != nil {
		return "", err
	}
	defer closeFn()

	entries, err := fs.ReadDir("/dists")
	if err != nil {
		return "", fmt.Errorf("read /dists from ISO %q: %w", ubuntuISO, err)
	}
	var candidates []string
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name())
		if name == "" || strings.EqualFold(name, "stable") || strings.EqualFold(name, "unstable") {
			continue
		}
		data, err := common.ReadISOFile(ubuntuISO, "/dists/"+name+"/Release")
		if err != nil {
			continue
		}
		if codename := releaseCodename(data); codename != "" {
			candidates = append(candidates, codename)
		} else {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", fmt.Errorf("could not detect Ubuntu codename from ISO %q", ubuntuISO)
	}
	return candidates[0], nil
}

func TransformUserData(userData []byte, install InstallConfig) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(userData, &root); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}

	autoinstall, err := common.AutoinstallMappingFromRoot(&root)
	if err != nil {
		return nil, err
	}
	if err := PrependInstallLateCommands(autoinstall, install); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		_ = encoder.Close()
		return nil, fmt.Errorf("encode YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("finish YAML encoding: %w", err)
	}
	return common.EnsureCloudConfigHeader(out.Bytes()), nil
}

func PrependInstallLateCommands(autoinstall *yaml.Node, install InstallConfig) error {
	if !install.Enabled() {
		return nil
	}
	if err := install.Validate(); err != nil {
		return err
	}
	lateCommands := common.MappingValue(autoinstall, "late-commands")
	if lateCommands == nil {
		lateCommands = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		autoinstall.Content = append(autoinstall.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "late-commands"},
			lateCommands,
		)
	} else if lateCommands.Kind != yaml.SequenceNode {
		return errors.New("autoinstall.late-commands must be a sequence when present")
	}

	var nodes []*yaml.Node
	for _, command := range InstallLateCommands(install) {
		nodes = append(nodes, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: command})
	}
	lateCommands.Content = append(nodes, lateCommands.Content...)
	return nil
}

func InstallLateCommands(install InstallConfig) []string {
	if !install.Enabled() {
		return nil
	}

	return []string{strings.Join([]string{
		"sh",
		shellQuote("/cdrom" + ISOInstallScriptPath),
		shellQuote("/target"),
		shellQuote("/cdrom" + ISORepoPath),
		shellQuote("/cdrom" + ISOInstallConfigPath),
	}, " ")}
}

func WriteInstallConfigDir(path string, install InstallConfig) error {
	if !install.Enabled() {
		return nil
	}
	if err := install.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create offline APT install config directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, "packages"), []byte(strings.Join(install.Packages, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write offline APT package list: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, "sources.list"), []byte(strings.Join(installSourceLines(install), "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write offline APT sources.list: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, "required-indexes"), []byte(strings.Join(installRequiredIndexPaths(install), "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("write offline APT required indexes: %w", err)
	}
	return nil
}

func installSourceLines(install InstallConfig) []string {
	var lines []string
	for _, source := range install.Sources {
		components := strings.Join(source.Components, " ")
		lines = append(lines, fmt.Sprintf("deb [trusted=yes check-date=no] file:%s %s %s", guestRepoPath, source.Suite, components))
	}
	return lines
}

func installRequiredIndexPaths(install InstallConfig) []string {
	var paths []string
	for _, source := range install.Sources {
		paths = append(paths, fmt.Sprintf("dists/%s/Release", source.Suite))
		for _, component := range source.Components {
			paths = append(paths, fmt.Sprintf("dists/%s/%s/binary-%s/Packages", source.Suite, component, aptArchitecture))
			paths = append(paths, fmt.Sprintf("dists/%s/%s/binary-%s/Packages", source.Suite, component, aptAllArchitecture))
		}
	}
	return paths
}

func ValidateRepository(repo Repository) error {
	if strings.TrimSpace(repo.Path) == "" {
		return errors.New("offline APT repository path is empty")
	}
	if info, err := os.Stat(repo.Path); err != nil {
		return fmt.Errorf("offline APT repository %q: %w", repo.Path, err)
	} else if !info.IsDir() {
		return fmt.Errorf("offline APT repository %q is not a directory", repo.Path)
	}
	if len(repo.Sources) == 0 {
		return errors.New("offline APT repository has no package index sources")
	}
	for _, source := range repo.Sources {
		if err := requireFile(filepath.Join(repo.Path, "dists", source.Suite, "Release")); err != nil {
			return err
		}
		for _, component := range source.Components {
			if err := requireFile(filepath.Join(repo.Path, "dists", source.Suite, component, "binary-"+aptArchitecture, "Packages")); err != nil {
				return err
			}
			if err := requireFile(filepath.Join(repo.Path, "dists", source.Suite, component, "binary-"+aptAllArchitecture, "Packages")); err != nil {
				return err
			}
		}
	}
	if len(repo.PackageFiles) == 0 {
		return errors.New("offline APT repository has no selected package files")
	}
	for _, rel := range repo.PackageFiles {
		if err := requireFile(filepath.Join(repo.Path, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	return nil
}

func ValidateEmbeddedRepository(isoPath string, repo Repository) error {
	if strings.TrimSpace(repo.Path) == "" {
		return nil
	}
	fs, closeFn, err := common.OpenISOFilesystem(isoPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if len(repo.Sources) == 0 {
		return errors.New("embedded offline APT repository has no package index sources")
	}

	var paths []string
	for _, source := range repo.Sources {
		paths = append(paths, isoRepoPath("dists/"+source.Suite+"/Release"))
		for _, component := range source.Components {
			paths = append(paths, isoRepoPath(fmt.Sprintf("dists/%s/%s/binary-%s/Packages", source.Suite, component, aptArchitecture)))
			paths = append(paths, isoRepoPath(fmt.Sprintf("dists/%s/%s/binary-%s/Packages", source.Suite, component, aptAllArchitecture)))
		}
	}
	for _, rel := range repo.PackageFiles {
		paths = append(paths, isoRepoPath(rel))
	}

	for _, path := range paths {
		file, err := common.OpenISOFile(fs, path)
		if err != nil {
			return fmt.Errorf("validate embedded offline APT repo: missing %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("validate embedded offline APT repo: close %s: %w", path, err)
		}
	}
	return nil
}

func parsePackageURIs(data []byte) ([]packageURI, error) {
	seen := map[string]bool{}
	var out []packageURI
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "'") {
			continue
		}
		end := strings.Index(line[1:], "'")
		if end < 0 {
			return nil, fmt.Errorf("parse apt package URI line %q: missing closing quote", line)
		}
		rawURI := line[1 : end+1]
		rest := strings.TrimSpace(line[end+2:])
		fields := strings.Fields(rest)
		downloadName := ""
		if len(fields) > 0 {
			downloadName = filepath.Base(fields[0])
		}

		rel, err := packageURIPath(rawURI)
		if err != nil {
			return nil, err
		}
		if downloadName == "" {
			downloadName = filepath.Base(rel)
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, packageURI{URI: rawURI, DownloadName: downloadName, RelativePath: rel})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RelativePath < out[j].RelativePath
	})
	return out, nil
}

func packageURIPath(rawURI string) (string, error) {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return "", fmt.Errorf("parse package URI %q: %w", rawURI, err)
	}
	uriPath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("decode package URI path %q: %w", rawURI, err)
	}
	idx := strings.Index(uriPath, "/pool/")
	if idx < 0 {
		return "", fmt.Errorf("package URI %q does not contain /pool/", rawURI)
	}
	rel := strings.TrimPrefix(uriPath[idx+1:], "/")
	rel = filepath.ToSlash(filepath.Clean(rel))
	if !strings.HasPrefix(rel, "pool/") || strings.Contains(rel, "../") {
		return "", fmt.Errorf("package URI %q resolved to unsafe path %q", rawURI, rel)
	}
	return rel, nil
}

func copySelectedPackages(downloadDir, repoDir string, packages []packageURI) error {
	for _, pkg := range packages {
		source := filepath.Join(downloadDir, filepath.Base(pkg.DownloadName))
		if _, err := os.Stat(source); err != nil {
			alternate := filepath.Join(downloadDir, filepath.Base(pkg.RelativePath))
			if _, altErr := os.Stat(alternate); altErr == nil {
				source = alternate
			} else {
				return fmt.Errorf("downloaded package %s not found in %s: %w", pkg.DownloadName, downloadDir, err)
			}
		}
		if err := copyFile(source, filepath.Join(repoDir, filepath.FromSlash(pkg.RelativePath)), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeTrimmedRepositoryMetadata(aptRoot, repoDir string, packages []packageURI) error {
	records, err := selectedPackageIndexRecords(aptRoot, packages)
	if err != nil {
		return err
	}

	recordsByArch := map[string][]packageIndexRecord{
		aptArchitecture:    nil,
		aptAllArchitecture: nil,
	}
	for _, record := range records {
		switch record.Architecture {
		case aptArchitecture, aptAllArchitecture:
			recordsByArch[record.Architecture] = append(recordsByArch[record.Architecture], record)
		default:
			return fmt.Errorf("downloaded package %s has unsupported architecture %q", record.RelativePath, record.Architecture)
		}
	}

	for _, arch := range []string{aptArchitecture, aptAllArchitecture} {
		path := filepath.Join(repoDir, "dists", offlineSuite, offlineComponent, "binary-"+arch, "Packages")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create parent directory for %s: %w", path, err)
		}
		var out strings.Builder
		for _, record := range recordsByArch[arch] {
			text := strings.TrimRight(record.Text, "\r\n")
			if text == "" {
				continue
			}
			out.WriteString(text)
			out.WriteString("\n\n")
		}
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			return fmt.Errorf("write trimmed package index %s: %w", path, err)
		}
	}

	return writeReleaseFile(repoDir)
}

func selectedPackageIndexRecords(aptRoot string, packages []packageURI) ([]packageIndexRecord, error) {
	want := map[string]bool{}
	for _, pkg := range packages {
		want[pkg.RelativePath] = true
	}
	if len(want) == 0 {
		return nil, errors.New("no downloaded package paths to index")
	}

	listDir := filepath.Join(aptRoot, "var/lib/apt/lists")
	entries, err := os.ReadDir(listDir)
	if err != nil {
		return nil, fmt.Errorf("read APT list directory %q: %w", listDir, err)
	}

	found := map[string]packageIndexRecord{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.Contains(entry.Name(), "_Packages") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(listDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read APT package index %s: %w", entry.Name(), err)
		}
		for _, paragraph := range packageIndexParagraphs(string(data)) {
			filename := normalizePackageFilename(packageIndexField(paragraph, "Filename"))
			if !want[filename] || found[filename].Text != "" {
				continue
			}
			architecture := strings.TrimSpace(packageIndexField(paragraph, "Architecture"))
			if architecture == "" {
				return nil, fmt.Errorf("package index record for %s has no Architecture field", filename)
			}
			found[filename] = packageIndexRecord{
				RelativePath: filename,
				Architecture: architecture,
				Text:         paragraph,
			}
		}
	}

	paths := packageRelativePaths(packages)
	records := make([]packageIndexRecord, 0, len(paths))
	for _, rel := range paths {
		record := found[rel]
		if record.Text == "" {
			return nil, fmt.Errorf("downloaded package %s was not found in authenticated APT package indexes", rel)
		}
		records = append(records, record)
	}
	return records, nil
}

func packageIndexParagraphs(data string) []string {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	parts := strings.Split(data, "\n\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, "\n")
		if strings.TrimSpace(part) != "" {
			out = append(out, part)
		}
	}
	return out
}

func packageIndexField(paragraph, name string) string {
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(paragraph, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.ToLower(strings.TrimSpace(key))+":" != prefix {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

func normalizePackageFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "./")
	value = filepath.ToSlash(filepath.Clean(value))
	if value == "." {
		return ""
	}
	return value
}

func writeReleaseFile(repoDir string) error {
	type releaseIndex struct {
		path string
		data []byte
	}
	var indexes []releaseIndex
	for _, arch := range []string{aptArchitecture, aptAllArchitecture} {
		rel := fmt.Sprintf("%s/binary-%s/Packages", offlineComponent, arch)
		path := filepath.Join(repoDir, "dists", offlineSuite, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read trimmed package index %s: %w", path, err)
		}
		indexes = append(indexes, releaseIndex{path: rel, data: data})
	}

	var out strings.Builder
	out.WriteString("Origin: ubuntu-vm-template-builder\n")
	out.WriteString("Label: ubuntu-vm-template-builder offline packages\n")
	out.WriteString("Suite: " + offlineSuite + "\n")
	out.WriteString("Codename: " + offlineSuite + "\n")
	out.WriteString("Architectures: " + aptArchitecture + " " + aptAllArchitecture + "\n")
	out.WriteString("Components: " + offlineComponent + "\n")
	out.WriteString("Date: Mon, 25 May 2026 00:00:00 +0000\n")
	for _, section := range []struct {
		name string
		sum  func([]byte) string
	}{
		{name: "MD5Sum", sum: func(data []byte) string { return fmt.Sprintf("%x", md5.Sum(data)) }},
		{name: "SHA1", sum: func(data []byte) string { return fmt.Sprintf("%x", sha1.Sum(data)) }},
		{name: "SHA256", sum: func(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }},
		{name: "SHA512", sum: func(data []byte) string { return fmt.Sprintf("%x", sha512.Sum512(data)) }},
	} {
		out.WriteString(section.name + ":\n")
		for _, index := range indexes {
			out.WriteString(fmt.Sprintf(" %s %d %s\n", section.sum(index.data), len(index.data), index.path))
		}
	}

	path := filepath.Join(repoDir, "dists", offlineSuite, "Release")
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return fmt.Errorf("write offline APT Release file: %w", err)
	}
	return nil
}

func checkAPTUpdateOutput(data []byte) error {
	text := string(data)
	for _, marker := range []string{
		"Failed to fetch",
		"Some index files failed to download",
		"NO_PUBKEY",
		"EXPKEYSIG",
		"BADSIG",
		"The following signatures couldn't be verified",
		"is not signed",
		"GPG error",
	} {
		if strings.Contains(text, marker) {
			return fmt.Errorf("apt-get update did not fully authenticate/download requested indexes: %s", strings.TrimSpace(text))
		}
	}
	return nil
}

func packageRelativePaths(packages []packageURI) []string {
	out := make([]string, 0, len(packages))
	for _, pkg := range packages {
		out = append(out, pkg.RelativePath)
	}
	sort.Strings(out)
	return out
}

func offlineRepositorySources() []RepositorySource {
	return []RepositorySource{{Suite: offlineSuite, Components: []string{offlineComponent}}}
}

func copySources(sources []RepositorySource) []RepositorySource {
	out := make([]RepositorySource, 0, len(sources))
	for _, source := range sources {
		out = append(out, RepositorySource{
			Suite:      source.Suite,
			Components: append([]string(nil), source.Components...),
		})
	}
	return out
}

func releaseCodename(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Codename") {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

func normalizeList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func validateAPTName(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%ss must not contain empty entries", kind)
	}
	if strings.ContainsAny(value, " \t\r\n/\\") || value == "." || value == ".." || strings.Contains(value, "..") {
		return fmt.Errorf("%s %q must not contain whitespace, path separators, or ..", kind, value)
	}
	return nil
}

func prepareAPTDirectories(downloadDir, aptRoot string) error {
	for _, dir := range []string{
		downloadDir,
		filepath.Join(downloadDir, "partial"),
		filepath.Join(aptRoot, "etc/apt/sources.list.d"),
		filepath.Join(aptRoot, "etc/apt/trusted.gpg.d"),
		filepath.Join(aptRoot, "etc/apt/preferences.d"),
		filepath.Join(aptRoot, "var/cache/apt/archives/partial"),
		filepath.Join(aptRoot, "var/lib/apt/lists/partial"),
		filepath.Join(aptRoot, "var/lib/dpkg"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	statusPath := filepath.Join(aptRoot, "var/lib/dpkg/status")
	if err := os.WriteFile(statusPath, nil, 0o644); err != nil {
		return fmt.Errorf("write empty dpkg status %s: %w", statusPath, err)
	}
	return nil
}

func writeAPTSources(path string, cfg Config, suites []string) error {
	var lines []string
	components := strings.Join(cfg.Components, " ")
	for _, suite := range suites {
		lines = append(lines, fmt.Sprintf("deb [signed-by=%s] %s %s %s", ubuntuArchiveKeyring, cfg.APTURL, suite, components))
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func expandSuites(suites []string, codename string) []string {
	var out []string
	for _, suite := range suites {
		switch suite {
		case "release":
			out = append(out, codename)
		case "updates":
			out = append(out, codename+"-updates")
		case "security":
			out = append(out, codename+"-security")
		case "backports":
			out = append(out, codename+"-backports")
		default:
			out = append(out, suite)
		}
	}
	return out
}

func isolatedAPTOptions(aptRoot, downloadDir string) []string {
	return []string{
		"-o", "Dir=" + aptRoot,
		"-o", "Dir::Etc=etc/apt",
		"-o", "Dir::Etc::sourcelist=sources.list",
		"-o", "Dir::Etc::sourceparts=sources.list.d",
		"-o", "Dir::Etc::trusted=trusted.gpg",
		"-o", "Dir::Etc::trustedparts=trusted.gpg.d",
		"-o", "Dir::State=var/lib/apt",
		"-o", "Dir::State::status=" + filepath.Join(aptRoot, "var/lib/dpkg/status"),
		"-o", "Dir::Cache=var/cache/apt",
		"-o", "Dir::Cache::archives=" + downloadDir,
		"-o", "APT::Architecture=" + aptArchitecture,
		"-o", "Debug::NoLocking=true",
		"-o", "Acquire::Languages=none",
		"-o", dep11IndexTargetOption,
		"-o", cnfIndexTargetOption,
	}
}

func requireFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("required offline APT repository file %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("required offline APT repository file %q is a directory", path)
	}
	return nil
}

func isoRepoPath(rel string) string {
	return strings.TrimRight(ISORepoPath, "/") + "/" + strings.TrimLeft(filepath.ToSlash(rel), "/")
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func commandString(name string, args []string) string {
	var parts []string
	parts = append(parts, shellQuote(name))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}
