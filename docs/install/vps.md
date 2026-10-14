# Choosing a VPS and its firewall

isshoni is made for a small cloud server (VPS) with a public IP address. This page helps you pick one, and says for
each provider where its firewall is and what to open. `isshoni doctor` and the setup wizard link to the section of the
provider they detect.

## What the server needs {#sizing}

isshoni forwards video and never re-encodes it, so the server needs little CPU and memory and a lot of **transfer**.
The size the M1 release is tested on:

- 2 vCPU (shared is fine) and 2 GB of RAM;
- a public IPv4 address (IPv6 as well is good);
- **at least 1 TB of transfer a month, and no per-GB billing for outgoing traffic**: that is the cost to watch. VPS
  plans of about €4 to €6 a month at the usual providers have this.

**How much transfer.** Every viewer gets the share they focus at full quality and the others as small previews:

```
egress ≈ Σ viewers × (focused share + thumbnails × preview)
```

with about 8 Mbps for a 1080p60 share (16 for 1440p60, 32 for 2160p60), 0.3 Mbps per preview and 128 kbps of sound.

| Group | Server egress | A 2-hour session |
|---|---|---|
| 5 people, 2 of them sharing (1080p60) | about 41.5 Mbps | about 39 GB |
| 10 people, all sharing, each watching 1 focused share and 8 thumbnails | about 105 Mbps | about 99 GB |

So a group that meets for two hours twice a week uses a few hundred GB a month. `isshoni doctor` computes it for your
group (`sudo isshoni doctor --people 8 --sharing 3 --hours 2`), and the admin page can warn you as the month's
transfer nears a limit you set (`limits.transfer_alert_gb`).

## Ports {#ports}

| Port | Protocol | What for | Needed |
|---|---|---|---|
| 80 | TCP | Let's Encrypt's HTTP challenge, and the redirect to HTTPS | yes, except [behind your own proxy](/install/reverse-proxy) |
| 443 | TCP | The web app, signaling, and media over TCP for networks that block everything else | yes, except behind your own proxy |
| 7882 | UDP | Media (video and sound) | yes |
| 7882 | TCP | Media over TCP | yes |

Open them for IPv4 and, where the server has it, IPv6, from anywhere. The install script opens them in ufw or
firewalld on the server; **your provider's cloud firewall is yours to open**, as below. The setup wizard's connection
test then checks from your browser that media gets through.

## Amazon Web Services (EC2) {#aws}

- **Firewall**: the instance's security group (EC2 console → Instances → the instance → Security → Security groups →
  Edit inbound rules). A new instance's group allows SSH only.
- **Open**: TCP 80, 443, 7882 and UDP 7882 from `0.0.0.0/0`, and the same from `::/0` if the instance has IPv6: AWS
  needs separate rules for IPv4 and IPv6.
- **Before installing without a domain, attach an Elastic IP.** The default public IPv4 address changes when the
  instance stops and starts. With a certificate for the IP address that moves the server, and breaks every friend's
  link, installed app and push subscription. A domain avoids it too.
- **Cost**: EC2 bills outgoing traffic per GB; a 2-hour session of 5 people is about 40 GB. Lightsail (below)
  includes a transfer allowance.

## AWS Lightsail {#aws-lightsail}

- **Firewall**: the instance's Networking tab. It has **separate IPv4 and IPv6 firewalls**: add the rules to both.
- **Open**: TCP 80, 443, 7882 and UDP 7882 (keep SSH).
- **Before installing without a domain, attach a static IP**, for the same reason as on EC2.

## Google Cloud {#gcp}

- **Firewall**: VPC network → Firewall. Create a rule for incoming traffic that allows TCP 80, 443, 7882 and UDP 7882
  from `0.0.0.0/0`, and give it a target tag that you also put on the VM (Compute Engine → the VM → Edit → Network
  tags). The VM's "Allow HTTP traffic" and "Allow HTTPS traffic" boxes open 80 and 443 only, not 7882.
- **Before installing without a domain, reserve a static external IP** (VPC network → IP addresses): the default one
  is ephemeral and changes when the VM stops, as on AWS.
- **Cost**: outgoing traffic is billed per GB.

## Microsoft Azure {#azure}

- **Firewall**: the network security group (NSG) on the VM's network interface or its subnet (the VM → Networking →
  Add inbound port rule). A new VM allows only the ports you picked when you created it.
- **Open**: TCP 80, 443, 7882 and UDP 7882. If both the interface and the subnet have an NSG, both must allow them.

## Oracle Cloud {#oracle}

- **Two firewalls**: the VCN's security list (Networking → Virtual cloud networks → the VCN → Security lists →
  Ingress rules) or a network security group, **and** the image's own iptables rules, which reject everything but
  SSH.
- **Open**: TCP 80, 443, 7882 and UDP 7882 in the security list. The install script offers to open the image's
  iptables (Ubuntu images) or firewalld (Oracle Linux) rules for you.
- **Free tier**: the Always Free Ampere instances (arm64) are a good fit; isshoni has arm64 builds.

## Hetzner {#hetzner}

- **Firewall**: none unless you add one. If you use a Cloud Firewall (Cloud Console → Firewalls), allow TCP 80, 443,
  7882 and UDP 7882 and apply it to the server.
- Outgoing traffic is included up to a large allowance per server.

## DigitalOcean {#digitalocean}

- **Firewall**: none unless you add one. If the Droplet has a Cloud Firewall (Networking → Firewalls), allow TCP 80,
  443, 7882 and UDP 7882 in its inbound rules.

## Vultr {#vultr}

- **Firewall**: none unless you link a firewall group to the instance (the customer portal's Firewall page); then
  allow TCP 80, 443, 7882 and UDP 7882 in it.
- Some Vultr images turn on ufw on the server; the install script offers to open the ports there.

## Akamai Cloud (Linode) {#linode}

- **Firewall**: none unless you add one. If the Linode has a Cloud Firewall (Cloud Manager → Firewalls), allow TCP 80,
  443, 7882 and UDP 7882 in its inbound rules.

## Scaleway {#scaleway}

- **Firewall**: the instance's security group (Instances → Security groups). Check that its inbound policy or rules
  allow TCP 80, 443, 7882 and UDP 7882.

## OVHcloud {#ovh}

- **Firewall**: none unless you add one. If you use the network firewall of a VPS or dedicated server, or a security
  group on a Public Cloud instance, allow TCP 80, 443, 7882 and UDP 7882 in it.

## Alibaba Cloud {#alibaba}

- **Firewall**: the instance's security group (ECS console → the instance → Security groups → Inbound rules). Add
  rules for TCP 80, 443, 7882 and UDP 7882.

## Tencent Cloud {#tencent}

- **Firewall**: the instance's security group (CVM console → the instance → Security groups → Inbound rules), or the
  firewall of a Lighthouse instance. Add rules for TCP 80, 443, 7882 and UDP 7882.

## Contabo {#contabo}

- **Firewall**: none unless you add one. Open the ports on the server if it runs a firewall; the install script offers
  to.

## Other providers {#unknown}

Look for a "firewall", "security group" or "network ACL" in your provider's control panel, and allow TCP 80, 443, 7882
and UDP 7882 from anywhere, for IPv4 and IPv6. If the server runs its own firewall (ufw, firewalld, iptables), open
the ports there too; the install script offers to.

Then run the connection test in the setup wizard (or later from the account menu, "Test my connection"): it checks
from your browser that media gets through.

## A server at home {#home}

isshoni works at home if the router gives you a public IPv4 address:

- **Forward** TCP 80, 443, 7882 and UDP 7882 on your router to the machine running isshoni.
- **Use a domain with dynamic DNS.** A certificate for the IP address breaks when your home IP address changes, and
  every friend's link with it.
- **CGNAT**: some internet providers, mobile ones especially, share one public address between many customers. Then no
  port forward can reach your machine, and `isshoni doctor` says so (`public_ip`). isshoni needs a VPS there, or a
  public address from your provider.
- On a Mac, browsers on the same network as the server need Local Network access
  ([troubleshooting](/troubleshooting#ct-mac_local_network)).
