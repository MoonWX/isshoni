package netx

import (
	"io/fs"
	"strings"

	"github.com/MoonWX/isshoni/internal/protocol/api"
)

// DMIDir is where Linux exposes the DMI strings DetectCloudProvider reads.
const DMIDir = "/sys/class/dmi/id"

// DetectCloudProvider identifies the hosting provider from DMI data only, with no metadata-service call (04 §13.3).
// dmi is rooted at DMIDir (os.DirFS(DMIDir) in production; a fstest.MapFS in tests); missing files count as
// empty, so other systems get api.CloudProviderUnknown.
func DetectCloudProvider(dmi fs.FS) api.CloudProvider {
	read := func(name string) string {
		if dmi == nil {
			return ""
		}
		b, err := fs.ReadFile(dmi, name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	vendor, bios, tag := read("sys_vendor"), read("bios_vendor"), read("chassis_asset_tag")
	switch {
	case vendor == "Amazon EC2" || strings.Contains(bios, "Amazon"):
		return api.CloudProviderAWS
	case vendor == "Google":
		return api.CloudProviderGCP
	case tag == "7783-7084-3265-9085-8269-3286-77":
		return api.CloudProviderAzure
	case tag == "OracleCloud.com":
		return api.CloudProviderOracle
	case vendor == "Hetzner":
		return api.CloudProviderHetzner
	case vendor == "DigitalOcean":
		return api.CloudProviderDigitalOcean
	case vendor == "Vultr":
		return api.CloudProviderVultr
	case strings.Contains(vendor, "Linode") || strings.Contains(vendor, "Akamai"):
		return api.CloudProviderLinode
	case vendor == "Scaleway":
		return api.CloudProviderScaleway
	case strings.Contains(vendor, "OVH"):
		return api.CloudProviderOVH
	case vendor == "Alibaba Cloud":
		return api.CloudProviderAlibaba
	case vendor == "Tencent Cloud":
		return api.CloudProviderTencent
	}
	return api.CloudProviderUnknown
}
