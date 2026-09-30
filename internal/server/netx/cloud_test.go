package netx

import (
	"testing"
	"testing/fstest"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// TestDetectCloudProvider covers each row of 04 §13.3.
func TestDetectCloudProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  api.CloudProvider
	}{
		{"aws vendor", map[string]string{"sys_vendor": "Amazon EC2\n"}, api.CloudProviderAWS},
		{"aws bios", map[string]string{"sys_vendor": "Xen", "bios_vendor": "Amazon EC2"}, api.CloudProviderAWS},
		{"gcp", map[string]string{"sys_vendor": "Google\n"}, api.CloudProviderGCP},
		{"azure", map[string]string{"sys_vendor": "Microsoft Corporation", "chassis_asset_tag": "7783-7084-3265-9085-8269-3286-77\n"}, api.CloudProviderAzure},
		{"oracle", map[string]string{"sys_vendor": "QEMU", "chassis_asset_tag": "OracleCloud.com"}, api.CloudProviderOracle},
		{"hetzner", map[string]string{"sys_vendor": "Hetzner\n"}, api.CloudProviderHetzner},
		{"digitalocean", map[string]string{"sys_vendor": "DigitalOcean"}, api.CloudProviderDigitalOcean},
		{"vultr", map[string]string{"sys_vendor": "Vultr"}, api.CloudProviderVultr},
		{"linode", map[string]string{"sys_vendor": "Linode"}, api.CloudProviderLinode},
		{"akamai", map[string]string{"sys_vendor": "Akamai Connected Cloud"}, api.CloudProviderLinode},
		{"scaleway", map[string]string{"sys_vendor": "Scaleway"}, api.CloudProviderScaleway},
		{"ovh", map[string]string{"sys_vendor": "OVH SAS"}, api.CloudProviderOVH},
		{"alibaba", map[string]string{"sys_vendor": "Alibaba Cloud"}, api.CloudProviderAlibaba},
		{"tencent", map[string]string{"sys_vendor": "Tencent Cloud"}, api.CloudProviderTencent},
		{"bare metal", map[string]string{"sys_vendor": "Dell Inc.", "bios_vendor": "Dell Inc."}, api.CloudProviderUnknown},
		{"no DMI", nil, api.CloudProviderUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name, data := range tc.files {
				fsys[name] = &fstest.MapFile{Data: []byte(data)}
			}
			if got := DetectCloudProvider(fsys); got != tc.want {
				t.Errorf("DetectCloudProvider = %q, want %q", got, tc.want)
			}
		})
	}
	if got := DetectCloudProvider(nil); got != api.CloudProviderUnknown {
		t.Errorf("DetectCloudProvider(nil) = %q", got)
	}
}
