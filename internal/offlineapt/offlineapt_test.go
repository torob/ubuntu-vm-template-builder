package offlineapt

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"ubuntu-vm-template-builder/internal/common"
)

func TestConfigNormalizeKeepsEmptyConfigDisabled(t *testing.T) {
	cfg := (Config{}).Normalize()
	if cfg.Enabled() {
		t.Fatalf("empty normalized config is enabled: %+v", cfg)
	}
	if len(cfg.Components) != 0 || len(cfg.Suites) != 0 {
		t.Fatalf("empty config should not receive defaults: %+v", cfg)
	}
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extra-packages.yaml")
	if err := os.WriteFile(path, []byte(`
apt_url: " http://archive.ubuntu.com/ubuntu/ "
packages:
  - " git "
  - curl
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.APTURL != "http://archive.ubuntu.com/ubuntu" {
		t.Fatalf("APTURL = %q", cfg.APTURL)
	}
	if strings.Join(cfg.Packages, ",") != "git,curl" {
		t.Fatalf("Packages = %#v", cfg.Packages)
	}
	if strings.Join(cfg.Components, ",") != "main,restricted,universe,multiverse" {
		t.Fatalf("Components = %#v", cfg.Components)
	}
	if strings.Join(cfg.Suites, ",") != "release,updates,security" {
		t.Fatalf("Suites = %#v", cfg.Suites)
	}

	invalidPath := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalidPath, []byte("apt_url: http://archive.ubuntu.com/ubuntu\n"), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if _, err := LoadConfig(invalidPath); err == nil || !strings.Contains(err.Error(), "packages") {
		t.Fatalf("LoadConfig invalid error = %v, want packages error", err)
	}
}

func TestTransformUserDataPrependsSignedInstallCommandAndKeepsAutoinstallPackages(t *testing.T) {
	original := []byte(`#cloud-config
autoinstall:
  version: 1
  packages:
    - vim
  late-commands:
    - echo user command
`)
	originalCopy := append([]byte(nil), original...)
	install := InstallConfig{
		Packages: []string{"git", "curl"},
		Sources: []RepositorySource{
			{Suite: "noble", Components: []string{"main"}},
			{Suite: "noble-updates", Components: []string{"main"}},
		},
	}

	transformed, err := TransformUserData(original, install)
	if err != nil {
		t.Fatalf("TransformUserData returned error: %v", err)
	}
	if !bytes.Equal(original, originalCopy) {
		t.Fatal("TransformUserData modified input bytes")
	}

	autoinstall, err := common.ParseAutoinstallMapping(transformed)
	if err != nil {
		t.Fatalf("parse transformed user-data: %v", err)
	}
	packages := common.MappingValue(autoinstall, "packages")
	if packages == nil || packages.Kind != yaml.SequenceNode || len(packages.Content) != 1 || packages.Content[0].Value != "vim" {
		t.Fatalf("autoinstall.packages changed: %#v", packages)
	}

	commands := lateCommandValues(t, autoinstall)
	extraCommands := InstallLateCommands(install)
	if len(commands) != len(extraCommands)+1 {
		t.Fatalf("late-commands length = %d, want %d: %#v", len(commands), len(extraCommands)+1, commands)
	}
	if commands[0] != extraCommands[0] {
		t.Fatalf("first late command = %q, want %q", commands[0], extraCommands[0])
	}
	if commands[len(commands)-1] != "echo user command" {
		t.Fatalf("user late-command was not preserved last: %#v", commands)
	}
}

func TestInstallLateCommandsUseSelfContainedOfflineRepo(t *testing.T) {
	install := InstallConfig{
		Packages: []string{"git", "curl"},
		Sources:  offlineRepositorySources(),
	}
	commands := InstallLateCommands(install)
	if len(commands) != 1 {
		t.Fatalf("InstallLateCommands returned %d commands, want 1", len(commands))
	}
	command := commands[0]
	for _, want := range []string{
		"/cdrom/ubuntu-vm-template-builder/scripts/install-offline-packages.sh",
		"/cdrom/ubuntu-vm-template-builder/offline-apt",
		"/cdrom/ubuntu-vm-template-builder/offline-apt-install",
		"/target",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("offline install command missing %q in:\n%s", want, command)
		}
	}
	for _, absent := range []string{"trusted=yes", "mount --bind", "dpkg-scanpackages", "signed-by=", "-y install", "git", "curl"} {
		if strings.Contains(command, absent) {
			t.Fatalf("offline install command contains generated script detail %q:\n%s", absent, command)
		}
	}

	configDir := filepath.Join(t.TempDir(), "offline-apt-install")
	if err := WriteInstallConfigDir(configDir, install); err != nil {
		t.Fatalf("WriteInstallConfigDir returned error: %v", err)
	}
	packages := readTestFile(t, filepath.Join(configDir, "packages"))
	sources := readTestFile(t, filepath.Join(configDir, "sources.list"))
	requiredIndexes := readTestFile(t, filepath.Join(configDir, "required-indexes"))
	if packages != "git\ncurl\n" {
		t.Fatalf("packages config = %q", packages)
	}
	for _, want := range []string{
		"trusted=yes check-date=no",
		"file:/var/lib/ubuntu-vm-template-builder/offline-apt offline main",
	} {
		if !strings.Contains(sources, want) {
			t.Fatalf("offline sources config missing %q in:\n%s", want, sources)
		}
	}
	for _, want := range []string{
		"dists/offline/Release",
		"dists/offline/main/binary-amd64/Packages",
		"dists/offline/main/binary-all/Packages",
	} {
		if !strings.Contains(requiredIndexes, want) {
			t.Fatalf("offline required-indexes config missing %q in:\n%s", want, requiredIndexes)
		}
	}
}

func TestInstallConfigUsesEffectiveSources(t *testing.T) {
	install := InstallConfig{
		Packages: []string{"git"},
		Sources: []RepositorySource{
			{Suite: "noble", Components: []string{"main", "universe"}},
			{Suite: "noble-updates", Components: []string{"main"}},
		},
	}
	configDir := filepath.Join(t.TempDir(), "offline-apt-install")
	if err := WriteInstallConfigDir(configDir, install); err != nil {
		t.Fatalf("WriteInstallConfigDir returned error: %v", err)
	}
	sources := readTestFile(t, filepath.Join(configDir, "sources.list"))
	requiredIndexes := readTestFile(t, filepath.Join(configDir, "required-indexes"))
	for _, want := range []string{
		"file:/var/lib/ubuntu-vm-template-builder/offline-apt noble main universe",
		"file:/var/lib/ubuntu-vm-template-builder/offline-apt noble-updates main",
	} {
		if !strings.Contains(sources, want) {
			t.Fatalf("offline sources config missing %q in:\n%s", want, sources)
		}
	}
	for _, want := range []string{
		"dists/noble/main/binary-amd64/Packages",
		"dists/noble/main/binary-all/Packages",
		"dists/noble/universe/binary-amd64/Packages",
		"dists/noble/universe/binary-all/Packages",
		"dists/noble-updates/main/binary-amd64/Packages",
		"dists/noble-updates/main/binary-all/Packages",
	} {
		if !strings.Contains(requiredIndexes, want) {
			t.Fatalf("offline required-indexes config missing %q in:\n%s", want, requiredIndexes)
		}
	}
	if strings.Contains(requiredIndexes, "dists/noble-updates/universe/binary-amd64/Packages") {
		t.Fatalf("offline install config checks a source component that was not copied:\n%s", requiredIndexes)
	}
}

func TestBuildRepositoryWithRunnerCreatesSelfContainedTrimmedRepo(t *testing.T) {
	runner := &fakeRunner{t: t}
	workDir := t.TempDir()
	cfg := Config{
		APTURL:     "http://mirror.example/ubuntu",
		Packages:   []string{"git", "curl"},
		Components: []string{"main", "multiverse"},
		Suites:     []string{"release", "updates"},
	}

	repo, err := buildRepositoryWithCodename(context.Background(), cfg, "noble", workDir, runner)
	if err != nil {
		t.Fatalf("buildRepositoryWithCodename returned error: %v", err)
	}
	if repo.Path != filepath.Join(workDir, repositoryWorkDirName) {
		t.Fatalf("repo path = %q", repo.Path)
	}
	if strings.Join(repo.Packages, ",") != "git,curl" {
		t.Fatalf("repo packages = %#v", repo.Packages)
	}
	assertSources(t, repo.Sources, offlineRepositorySources())
	assertSources(t, repo.InstallConfig().Sources, offlineRepositorySources())

	sources, err := os.ReadFile(filepath.Join(workDir, aptWorkDirName, "etc/apt/sources.list"))
	if err != nil {
		t.Fatalf("read generated sources.list: %v", err)
	}
	for _, want := range []string{
		"deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] http://mirror.example/ubuntu noble main",
		"deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] http://mirror.example/ubuntu noble-updates main",
	} {
		if !strings.Contains(string(sources), want) {
			t.Fatalf("sources.list missing %q in:\n%s", want, sources)
		}
	}
	if strings.Contains(string(sources), "trusted=yes") {
		t.Fatalf("sources.list should not use trusted=yes:\n%s", sources)
	}

	for _, want := range []string{
		"dists/offline/Release",
		"dists/offline/main/binary-amd64/Packages",
		"dists/offline/main/binary-all/Packages",
		"pool/main/g/git/git_1_amd64.deb",
		"pool/main/c/curl/curl_1_all.deb",
	} {
		if _, err := os.Stat(filepath.Join(repo.Path, filepath.FromSlash(want))); err != nil {
			t.Fatalf("repo missing %s: %v", want, err)
		}
	}
	amd64Packages := readTestFile(t, filepath.Join(repo.Path, "dists/offline/main/binary-amd64/Packages"))
	allPackages := readTestFile(t, filepath.Join(repo.Path, "dists/offline/main/binary-all/Packages"))
	release := readTestFile(t, filepath.Join(repo.Path, "dists/offline/Release"))
	if !strings.Contains(amd64Packages, "Package: git") || strings.Contains(amd64Packages, "Package: curl") || strings.Contains(amd64Packages, "Package: vim") {
		t.Fatalf("amd64 Packages should contain only selected amd64 package:\n%s", amd64Packages)
	}
	if !strings.Contains(allPackages, "Package: curl") || strings.Contains(allPackages, "Package: git") || strings.Contains(allPackages, "Package: vim") {
		t.Fatalf("all Packages should contain only selected Architecture: all package:\n%s", allPackages)
	}
	for _, want := range []string{"MD5Sum:", "SHA1:", "SHA256:", "SHA512:", "main/binary-amd64/Packages", "main/binary-all/Packages"} {
		if !strings.Contains(release, want) {
			t.Fatalf("Release file missing %q in:\n%s", want, release)
		}
	}
	if _, err := os.Stat(filepath.Join(repo.Path, "Packages")); !os.IsNotExist(err) {
		t.Fatalf("repo should not contain generated flat Packages index, stat err = %v", err)
	}

	if !runner.sawCommand("apt-get", "update") {
		t.Fatalf("runner did not see apt-get update: %#v", runner.commands)
	}
	if !runner.sawCommandWithArgs("apt-get", "update", dep11IndexTargetOption, cnfIndexTargetOption) {
		t.Fatalf("runner apt-get update did not disable optional index targets: %#v", runner.commands)
	}
	if !runner.sawCommand("apt-get", "--print-uris") {
		t.Fatalf("runner did not see apt-get --print-uris: %#v", runner.commands)
	}
	if !runner.sawCommand("apt-get", "--download-only") {
		t.Fatalf("runner did not see apt-get --download-only: %#v", runner.commands)
	}
	if runner.sawCommand("dpkg-scanpackages", ".") {
		t.Fatalf("runner unexpectedly saw dpkg-scanpackages: %#v", runner.commands)
	}
}

func TestBuildRepositoryRejectsUnauthenticatedPackagesBeforeISOEmbedding(t *testing.T) {
	runner := &fakeRunner{
		t:            t,
		updateOutput: "W: GPG error: http://mirror.example/ubuntu noble InRelease: The following signatures couldn't be verified because the public key is not available: NO_PUBKEY 0123456789ABCDEF\n",
	}
	workDir := t.TempDir()

	_, err := buildRepositoryWithCodename(context.Background(), Config{
		APTURL:   "http://mirror.example/ubuntu",
		Packages: []string{"git"},
	}, "noble", workDir, runner)
	if err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("buildRepositoryWithCodename error = %v, want authentication failure", err)
	}
	if runner.sawCommand("apt-get", "--print-uris") || runner.sawCommand("apt-get", "--download-only") {
		t.Fatalf("unauthenticated package indexes should not be used for ISO repo contents: %#v", runner.commands)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, repositoryWorkDirName)); !os.IsNotExist(statErr) {
		t.Fatalf("unauthenticated packages should not produce a repo for remastered ISO embedding, stat err = %v", statErr)
	}
}

func TestParsePackageURIsKeepsOriginalPoolPaths(t *testing.T) {
	uris, err := parsePackageURIs([]byte(`'http://mirror.example/ubuntu/pool/main/g/git/git_1_amd64.deb' git_1_amd64.deb 1 MD5Sum:abc
'http://mirror.example/ubuntu/pool/main/c/curl/curl_1_amd64.deb' curl_1_amd64.deb 1 MD5Sum:def
`))
	if err != nil {
		t.Fatalf("parsePackageURIs returned error: %v", err)
	}
	got := packageRelativePaths(uris)
	want := []string{"pool/main/c/curl/curl_1_amd64.deb", "pool/main/g/git/git_1_amd64.deb"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("package paths = %#v, want %#v", got, want)
	}
}

func TestValidateRepositoryRejectsMissingSelectedDeb(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dists/offline/Release"), "Suite: offline\n")
	writeFile(t, filepath.Join(dir, "dists/offline/main/binary-amd64/Packages"), "Package: git\n")
	writeFile(t, filepath.Join(dir, "dists/offline/main/binary-all/Packages"), "")

	err := ValidateRepository(Repository{
		Path:         dir,
		Sources:      offlineRepositorySources(),
		PackageFiles: []string{"pool/main/g/git/git_1_amd64.deb"},
	})
	if err == nil || !strings.Contains(err.Error(), "git_1_amd64.deb") {
		t.Fatalf("ValidateRepository error = %v, want missing deb", err)
	}
}

type fakeRunner struct {
	t            *testing.T
	updateOutput string
	commands     []recordedCommand
}

type recordedCommand struct {
	dir  string
	name string
	args []string
}

func (r *fakeRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{
		dir:  dir,
		name: name,
		args: append([]string(nil), args...),
	})
	if name != "apt-get" {
		return nil, os.ErrInvalid
	}

	aptRoot := aptOptionValue(args, "Dir=")
	downloadDir := aptOptionValue(args, "Dir::Cache::archives=")
	switch {
	case containsArg(args, "update"):
		r.writeAPTLists(aptRoot)
		if r.updateOutput != "" {
			return []byte(r.updateOutput), nil
		}
		return []byte("ok\n"), nil
	case containsArg(args, "--print-uris"):
		return []byte(strings.Join([]string{
			"'http://mirror.example/ubuntu/pool/main/g/git/git_1_amd64.deb' git_1_amd64.deb 1 MD5Sum:abc",
			"'http://mirror.example/ubuntu/pool/main/c/curl/curl_1_all.deb' curl_1_all.deb 1 MD5Sum:def",
			"",
		}, "\n")), nil
	case containsArg(args, "--download-only"):
		writeFile(r.t, filepath.Join(downloadDir, "git_1_amd64.deb"), "git deb")
		writeFile(r.t, filepath.Join(downloadDir, "curl_1_all.deb"), "curl deb")
		return []byte("downloaded\n"), nil
	default:
		return []byte("ok\n"), nil
	}
}

func (r *fakeRunner) writeAPTLists(aptRoot string) {
	writePlainFile(filepath.Join(aptRoot, "var/lib/apt/lists/mirror_dists_noble_InRelease"), "signed noble\n")
	writePlainFile(filepath.Join(aptRoot, "var/lib/apt/lists/mirror_dists_noble-updates_InRelease"), "signed noble-updates\n")
	writePlainFile(filepath.Join(aptRoot, "var/lib/apt/lists/mirror_dists_noble_main_binary-amd64_Packages"), strings.Join([]string{
		"Package: git",
		"Architecture: amd64",
		"Filename: pool/main/g/git/git_1_amd64.deb",
		"",
		"Package: vim",
		"Architecture: amd64",
		"Filename: pool/main/v/vim/vim_1_amd64.deb",
		"",
	}, "\n"))
	writePlainFile(filepath.Join(aptRoot, "var/lib/apt/lists/mirror_dists_noble-updates_main_binary-amd64_Packages"), strings.Join([]string{
		"Package: curl",
		"Architecture: all",
		"Filename: pool/main/c/curl/curl_1_all.deb",
		"",
	}, "\n"))
}

func (r *fakeRunner) sawCommand(name, arg string) bool {
	for _, command := range r.commands {
		if command.name == name && containsArg(command.args, arg) {
			return true
		}
	}
	return false
}

func (r *fakeRunner) sawCommandWithArgs(name string, args ...string) bool {
	for _, command := range r.commands {
		if command.name != name {
			continue
		}
		matched := true
		for _, arg := range args {
			if !containsArg(command.args, arg) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func aptOptionValue(args []string, prefix string) string {
	for idx, arg := range args {
		if value, ok := strings.CutPrefix(arg, prefix); ok {
			return value
		}
		if arg == "-o" && idx+1 < len(args) {
			if value, ok := strings.CutPrefix(args[idx+1], prefix); ok {
				return value
			}
		}
	}
	return ""
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func lateCommandValues(t *testing.T, autoinstall *yaml.Node) []string {
	t.Helper()
	lateCommands := common.MappingValue(autoinstall, "late-commands")
	if lateCommands == nil || lateCommands.Kind != yaml.SequenceNode {
		t.Fatalf("late-commands = %#v, want sequence", lateCommands)
	}
	var commands []string
	for _, node := range lateCommands.Content {
		if node.Kind != yaml.ScalarNode {
			t.Fatalf("late-command node = %#v, want scalar", node)
		}
		commands = append(commands, node.Value)
	}
	return commands
}

func assertSources(t *testing.T, got, want []RepositorySource) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
	for idx := range want {
		if got[idx].Suite != want[idx].Suite || strings.Join(got[idx].Components, ",") != strings.Join(want[idx].Components, ",") {
			t.Fatalf("sources = %#v, want %#v", got, want)
		}
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent dir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writePlainFile(path, data string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		panic(err)
	}
}
