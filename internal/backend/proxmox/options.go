package proxmox

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const cloudInitDiskBytes = 4 * 1024 * 1024

type OptionsConfig struct {
	Enabled            bool                     `yaml:"-"`
	StartAtBoot        *bool                    `yaml:"start_at_boot"`
	Startup            StartupOptions           `yaml:"startup"`
	QEMUGuestAgent     QEMUGuestAgentOptions    `yaml:"qemu_guest_agent"`
	Protection         *bool                    `yaml:"protection"`
	Tablet             *bool                    `yaml:"tablet"`
	ACPI               *bool                    `yaml:"acpi"`
	KVM                *bool                    `yaml:"kvm"`
	FreezeCPUAtStartup *bool                    `yaml:"freeze_cpu_at_startup"`
	LocalTime          *bool                    `yaml:"local_time"`
	RTCStartDate       string                   `yaml:"rtc_start_date"`
	Hotplug            HotplugOptions           `yaml:"hotplug"`
	SMBIOS             SMBIOSOptions            `yaml:"smbios"`
	SPICEEnhancements  SPICEEnhancementsOptions `yaml:"spice_enhancements"`
	VMStateStorage     string                   `yaml:"vm_state_storage"`
	Tags               []string                 `yaml:"tags"`
	Description        string                   `yaml:"description"`
}

type StartupOptions struct {
	Order            *int `yaml:"order"`
	UpDelaySeconds   *int `yaml:"up_delay_seconds"`
	DownDelaySeconds *int `yaml:"down_delay_seconds"`
}

type QEMUGuestAgentOptions struct {
	Enabled           *bool  `yaml:"enabled"`
	FreezeFSOnBackup  *bool  `yaml:"freeze_fs_on_backup"`
	FstrimClonedDisks *bool  `yaml:"fstrim_cloned_disks"`
	Type              string `yaml:"type"`
}

type HotplugOptions struct {
	Network   *bool `yaml:"network"`
	Disk      *bool `yaml:"disk"`
	USB       *bool `yaml:"usb"`
	Memory    *bool `yaml:"memory"`
	CPU       *bool `yaml:"cpu"`
	CloudInit *bool `yaml:"cloudinit"`
}

type SMBIOSOptions struct {
	ValuesAreBase64 *bool  `yaml:"values_are_base64"`
	UUID            string `yaml:"uuid"`
	Manufacturer    string `yaml:"manufacturer"`
	Product         string `yaml:"product"`
	Version         string `yaml:"version"`
	Serial          string `yaml:"serial"`
	SKU             string `yaml:"sku"`
	Family          string `yaml:"family"`
}

type SPICEEnhancementsOptions struct {
	FolderSharing  *bool  `yaml:"folder_sharing"`
	VideoStreaming string `yaml:"video_streaming"`
}

type CloudInitOptionsConfig struct {
	Enabled  bool                 `yaml:"-"`
	Type     string               `yaml:"type"`
	Upgrade  *bool                `yaml:"upgrade"`
	User     string               `yaml:"user"`
	Password string               `yaml:"password"`
	SSHKeys  []string             `yaml:"ssh_keys"`
	DNS      CloudInitDNSOptions  `yaml:"dns"`
	Network  []CloudInitIPConfig  `yaml:"network"`
	Custom   CloudInitCustomFiles `yaml:"custom"`
}

type CloudInitDNSOptions struct {
	Nameservers   []string `yaml:"nameservers"`
	SearchDomains []string `yaml:"search_domains"`
}

type CloudInitIPConfig struct {
	Index    int    `yaml:"index"`
	IPv4     string `yaml:"ipv4"`
	Gateway4 string `yaml:"gateway4"`
	IPv6     string `yaml:"ipv6"`
	Gateway6 string `yaml:"gateway6"`
}

type CloudInitCustomFiles struct {
	User    string `yaml:"user"`
	Network string `yaml:"network"`
	Meta    string `yaml:"meta"`
	Vendor  string `yaml:"vendor"`
}

func LoadOptionsConfig(path string) (OptionsConfig, error) {
	var cfg OptionsConfig
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	if err := loadStrictYAML(path, "Proxmox options", &cfg); err != nil {
		return cfg, err
	}
	cfg.Enabled = true
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid Proxmox options config %q: %w", path, err)
	}
	return cfg, nil
}

func LoadCloudInitOptionsConfig(path string) (CloudInitOptionsConfig, error) {
	var cfg CloudInitOptionsConfig
	if strings.TrimSpace(path) == "" {
		return cfg, nil
	}
	if err := loadStrictYAML(path, "Proxmox cloud-init options", &cfg); err != nil {
		return cfg, err
	}
	cfg.Enabled = true
	cfg = cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid Proxmox cloud-init options config %q: %w", path, err)
	}
	return cfg, nil
}

func loadStrictYAML(path, label string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s config %q: %w", label, path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%s config %q is empty", label, path)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("parse %s config %q: %w", label, path, err)
	}
	return nil
}

func (c OptionsConfig) Normalize() OptionsConfig {
	c.QEMUGuestAgent.Type = strings.ToLower(strings.TrimSpace(c.QEMUGuestAgent.Type))
	c.RTCStartDate = strings.TrimSpace(c.RTCStartDate)
	c.SMBIOS.UUID = strings.TrimSpace(c.SMBIOS.UUID)
	c.SMBIOS.Manufacturer = strings.TrimSpace(c.SMBIOS.Manufacturer)
	c.SMBIOS.Product = strings.TrimSpace(c.SMBIOS.Product)
	c.SMBIOS.Version = strings.TrimSpace(c.SMBIOS.Version)
	c.SMBIOS.Serial = strings.TrimSpace(c.SMBIOS.Serial)
	c.SMBIOS.SKU = strings.TrimSpace(c.SMBIOS.SKU)
	c.SMBIOS.Family = strings.TrimSpace(c.SMBIOS.Family)
	c.SPICEEnhancements.VideoStreaming = strings.ToLower(strings.TrimSpace(c.SPICEEnhancements.VideoStreaming))
	c.VMStateStorage = strings.TrimSpace(c.VMStateStorage)
	for idx := range c.Tags {
		c.Tags[idx] = strings.TrimSpace(c.Tags[idx])
	}
	return c
}

func (c OptionsConfig) Validate() error {
	c = c.Normalize()
	if c.Startup.Order != nil && *c.Startup.Order < 0 {
		return fmt.Errorf("startup.order must be greater than or equal to zero")
	}
	if c.Startup.UpDelaySeconds != nil && *c.Startup.UpDelaySeconds < 0 {
		return fmt.Errorf("startup.up_delay_seconds must be greater than or equal to zero")
	}
	if c.Startup.DownDelaySeconds != nil && *c.Startup.DownDelaySeconds < 0 {
		return fmt.Errorf("startup.down_delay_seconds must be greater than or equal to zero")
	}
	if c.QEMUGuestAgent.Type != "" && !isOneOf(c.QEMUGuestAgent.Type, "virtio", "isa") {
		return fmt.Errorf("qemu_guest_agent.type must be virtio or isa")
	}
	if c.SPICEEnhancements.VideoStreaming != "" && !isOneOf(c.SPICEEnhancements.VideoStreaming, "off", "all", "filter") {
		return fmt.Errorf("spice_enhancements.video_streaming must be off, all, or filter")
	}
	for _, tag := range c.Tags {
		if tag == "" {
			return fmt.Errorf("tags must not contain empty values")
		}
		if strings.ContainsAny(tag, ",;") {
			return fmt.Errorf("tag %q must not contain comma or semicolon", tag)
		}
	}
	return nil
}

func (c CloudInitOptionsConfig) Normalize() CloudInitOptionsConfig {
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	c.User = strings.TrimSpace(c.User)
	for idx := range c.SSHKeys {
		c.SSHKeys[idx] = strings.TrimSpace(c.SSHKeys[idx])
	}
	for idx := range c.DNS.Nameservers {
		c.DNS.Nameservers[idx] = strings.TrimSpace(c.DNS.Nameservers[idx])
	}
	for idx := range c.DNS.SearchDomains {
		c.DNS.SearchDomains[idx] = strings.TrimSpace(c.DNS.SearchDomains[idx])
	}
	for idx := range c.Network {
		c.Network[idx].IPv4 = strings.TrimSpace(c.Network[idx].IPv4)
		c.Network[idx].Gateway4 = strings.TrimSpace(c.Network[idx].Gateway4)
		c.Network[idx].IPv6 = strings.TrimSpace(c.Network[idx].IPv6)
		c.Network[idx].Gateway6 = strings.TrimSpace(c.Network[idx].Gateway6)
	}
	c.Custom.User = strings.TrimSpace(c.Custom.User)
	c.Custom.Network = strings.TrimSpace(c.Custom.Network)
	c.Custom.Meta = strings.TrimSpace(c.Custom.Meta)
	c.Custom.Vendor = strings.TrimSpace(c.Custom.Vendor)
	return c
}

func (c CloudInitOptionsConfig) Validate() error {
	c = c.Normalize()
	if c.Type != "" && !isOneOf(c.Type, "nocloud", "configdrive2", "opennebula") {
		return fmt.Errorf("type must be nocloud, configdrive2, or opennebula")
	}
	for _, key := range c.SSHKeys {
		if key == "" {
			return fmt.Errorf("ssh_keys must not contain empty values")
		}
	}
	for _, value := range c.DNS.Nameservers {
		if value == "" {
			return fmt.Errorf("dns.nameservers must not contain empty values")
		}
	}
	for _, value := range c.DNS.SearchDomains {
		if value == "" {
			return fmt.Errorf("dns.search_domains must not contain empty values")
		}
	}
	seen := map[int]bool{}
	for _, nic := range c.Network {
		if nic.Index < 0 {
			return fmt.Errorf("network.index must be greater than or equal to zero")
		}
		if seen[nic.Index] {
			return fmt.Errorf("network.index %d is duplicated", nic.Index)
		}
		seen[nic.Index] = true
		if nic.Gateway4 != "" && nic.IPv4 == "" {
			return fmt.Errorf("network[%d].gateway4 requires ipv4", nic.Index)
		}
		if nic.Gateway6 != "" && nic.IPv6 == "" {
			return fmt.Errorf("network[%d].gateway6 requires ipv6", nic.Index)
		}
	}
	return nil
}

func BuildOptionsValues(cfg Config) (url.Values, error) {
	options := cfg.Options.Normalize()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	values := url.Values{}
	if options.StartAtBoot != nil {
		values.Set("onboot", boolString(*options.StartAtBoot))
	}
	if startup := buildStartupValue(options.Startup); startup != "" {
		values.Set("startup", startup)
	}
	if agent := buildAgentValue(options.QEMUGuestAgent); agent != "" {
		values.Set("agent", agent)
	}
	setBoolValue(values, "protection", options.Protection)
	setBoolValue(values, "tablet", options.Tablet)
	setBoolValue(values, "acpi", options.ACPI)
	setBoolValue(values, "kvm", options.KVM)
	setBoolValue(values, "freeze", options.FreezeCPUAtStartup)
	setBoolValue(values, "localtime", options.LocalTime)
	if options.RTCStartDate != "" {
		values.Set("startdate", options.RTCStartDate)
	}
	if hotplug := buildHotplugValue(options.Hotplug); hotplug != "" {
		values.Set("hotplug", hotplug)
	}
	if smbios := buildSMBIOSValue(options.SMBIOS); smbios != "" {
		values.Set("smbios1", smbios)
	}
	if spice := buildSPICEEnhancementsValue(options.SPICEEnhancements); spice != "" {
		values.Set("spice_enhancements", spice)
	}
	if options.VMStateStorage != "" {
		values.Set("vmstatestorage", options.VMStateStorage)
	}
	if len(options.Tags) > 0 {
		values.Set("tags", strings.Join(options.Tags, ";"))
	}
	if options.Description != "" {
		values.Set("description", options.Description)
	}
	return values, nil
}

func BuildCloudInitValues(cfg Config) (url.Values, error) {
	options := cfg.CloudInitOptions.Normalize()
	if err := options.Validate(); err != nil {
		return nil, err
	}
	connection := normalizeConnectionConfig(cfg.Proxmox, cfg.DisplayName)
	values := url.Values{}
	values.Set("ide2", fmt.Sprintf("%s:cloudinit", connection.DiskStorage))
	if options.Type != "" {
		values.Set("citype", options.Type)
	}
	if options.Upgrade != nil {
		values.Set("ciupgrade", boolString(*options.Upgrade))
	}
	if options.User != "" {
		values.Set("ciuser", options.User)
	}
	if options.Password != "" {
		values.Set("cipassword", options.Password)
	}
	if len(options.SSHKeys) > 0 {
		// Proxmox validates sshkeys as URL-encoded OpenSSH text inside the form value.
		values.Set("sshkeys", encodeProxmoxURLValue(strings.Join(options.SSHKeys, "\n")))
	}
	if len(options.DNS.Nameservers) > 0 {
		values.Set("nameserver", strings.Join(options.DNS.Nameservers, " "))
	}
	if len(options.DNS.SearchDomains) > 0 {
		values.Set("searchdomain", strings.Join(options.DNS.SearchDomains, " "))
	}
	for _, nic := range options.Network {
		if ipconfig := buildIPConfigValue(nic); ipconfig != "" {
			values.Set("ipconfig"+strconv.Itoa(nic.Index), ipconfig)
		}
	}
	if custom := buildCICustomValue(options.Custom); custom != "" {
		values.Set("cicustom", custom)
	}
	return values, nil
}

func buildStartupValue(options StartupOptions) string {
	var parts []string
	if options.Order != nil {
		parts = append(parts, "order="+strconv.Itoa(*options.Order))
	}
	if options.UpDelaySeconds != nil {
		parts = append(parts, "up="+strconv.Itoa(*options.UpDelaySeconds))
	}
	if options.DownDelaySeconds != nil {
		parts = append(parts, "down="+strconv.Itoa(*options.DownDelaySeconds))
	}
	return strings.Join(parts, ",")
}

func buildAgentValue(options QEMUGuestAgentOptions) string {
	var parts []string
	if options.Enabled != nil {
		parts = append(parts, "enabled="+boolString(*options.Enabled))
	}
	if options.FreezeFSOnBackup != nil {
		parts = append(parts, "freeze-fs-on-backup="+boolString(*options.FreezeFSOnBackup))
	}
	if options.FstrimClonedDisks != nil {
		parts = append(parts, "fstrim_cloned_disks="+boolString(*options.FstrimClonedDisks))
	}
	if options.Type != "" {
		parts = append(parts, "type="+options.Type)
	}
	return strings.Join(parts, ",")
}

func buildHotplugValue(options HotplugOptions) string {
	fields := []struct {
		name  string
		value *bool
	}{
		{name: "network", value: options.Network},
		{name: "disk", value: options.Disk},
		{name: "usb", value: options.USB},
		{name: "memory", value: options.Memory},
		{name: "cpu", value: options.CPU},
		{name: "cloudinit", value: options.CloudInit},
	}
	var enabled []string
	wasSet := false
	for _, field := range fields {
		if field.value == nil {
			continue
		}
		wasSet = true
		if *field.value {
			enabled = append(enabled, field.name)
		}
	}
	if !wasSet {
		return ""
	}
	if len(enabled) == 0 {
		return "0"
	}
	return strings.Join(enabled, ",")
}

func buildSMBIOSValue(options SMBIOSOptions) string {
	var parts []string
	if options.ValuesAreBase64 != nil {
		parts = append(parts, "base64="+boolString(*options.ValuesAreBase64))
	}
	appendKV := func(key, value string) {
		if value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	appendKV("uuid", options.UUID)
	appendKV("manufacturer", options.Manufacturer)
	appendKV("product", options.Product)
	appendKV("version", options.Version)
	appendKV("serial", options.Serial)
	appendKV("sku", options.SKU)
	appendKV("family", options.Family)
	return strings.Join(parts, ",")
}

func buildSPICEEnhancementsValue(options SPICEEnhancementsOptions) string {
	var parts []string
	if options.FolderSharing != nil {
		parts = append(parts, "foldersharing="+boolString(*options.FolderSharing))
	}
	if options.VideoStreaming != "" {
		parts = append(parts, "videostreaming="+options.VideoStreaming)
	}
	return strings.Join(parts, ",")
}

func buildIPConfigValue(options CloudInitIPConfig) string {
	var parts []string
	if options.IPv4 != "" {
		parts = append(parts, "ip="+options.IPv4)
	}
	if options.Gateway4 != "" {
		parts = append(parts, "gw="+options.Gateway4)
	}
	if options.IPv6 != "" {
		parts = append(parts, "ip6="+options.IPv6)
	}
	if options.Gateway6 != "" {
		parts = append(parts, "gw6="+options.Gateway6)
	}
	return strings.Join(parts, ",")
}

func buildCICustomValue(options CloudInitCustomFiles) string {
	partsByKey := map[string]string{}
	if options.User != "" {
		partsByKey["user"] = options.User
	}
	if options.Network != "" {
		partsByKey["network"] = options.Network
	}
	if options.Meta != "" {
		partsByKey["meta"] = options.Meta
	}
	if options.Vendor != "" {
		partsByKey["vendor"] = options.Vendor
	}
	if len(partsByKey) == 0 {
		return ""
	}
	keys := make([]string, 0, len(partsByKey))
	for key := range partsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+partsByKey[key])
	}
	return strings.Join(parts, ",")
}

func setBoolValue(values url.Values, key string, value *bool) {
	if value != nil {
		values.Set(key, boolString(*value))
	}
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func encodeProxmoxURLValue(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func isOneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
