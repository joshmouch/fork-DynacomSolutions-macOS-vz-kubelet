# macOS Virtualization Kubelet

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
![Platform](https://img.shields.io/badge/platform-macOS%20%C2%B7%20Apple%20Silicon-lightgrey.svg)

**Run native macOS virtual machines as Kubernetes pods.**

`macOS-vz-kubelet` is a [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) provider that
turns an Apple Silicon Mac into a Kubernetes node. Each pod's first container boots as a native macOS VM on
Apple's [Virtualization framework](https://developer.apple.com/documentation/virtualization); optional Docker
side-car containers run in the same pod for logging, monitoring, or artifact handling.

It targets teams scaling macOS CI/CD (GitLab runners, build and test farms) on a cluster, at near-native Apple
Silicon performance and without QEMU/KVM.

## Contents

- [Overview](#overview)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Getting started](#getting-started)
- [Networking](#networking)
- [Imaging](#imaging)
- [Feature overview](#feature-overview)
- [Configuration](#configuration)
- [Examples](#examples)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [Related projects](#related-projects)
- [License](#license)

## Overview

Running macOS under Kubernetes traditionally means QEMU/KVM on Linux, which cannot fully use Apple Silicon
hardware. `macOS-vz-kubelet` instead runs VMs natively through Virtualization.framework on the Mac itself, so
guests run at near-native speed.

The project is designed to:

- Run macOS VMs as first-class pods in a Kubernetes cluster.
- Support hybrid pods that combine a macOS VM with Docker side-car containers.
- Manage VM images with a custom, compressed, OCI-compliant format.
- Integrate with Kubernetes scheduling, resource requests, and lifecycle.

For exactly what is and is not supported, see the [Feature overview](#feature-overview).

## How it works

The kubelet runs on the Mac host as a Virtual Kubelet provider and orchestrates two backends from one pod
spec.

- **Virtualization framework** provisions and runs the macOS VM (pod container index 0), directly on Apple
  Silicon.
- **Custom OCI image format** distributes VM images. Images are built with Virtualization.framework-compatible
  tooling and pushed/pulled as compressed OCI artifacts; the kubelet pulls them via the
  [oras-go](https://github.com/oras-project/oras-go) library. Packaging/push uses a dedicated ORAS CLI fork,
  [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz).
- **Hybrid pods.** Container 0 is always the macOS VM. Containers 1..N run on the local Docker daemon as
  regular containers.
- **Networking** is wired automatically: local NAT by default, or bridged for routable IPs. See
  [Networking](#networking).
- **Resource and lifecycle.** CPU/memory requests size the VM. Pod create and delete are supported; updates
  require recreating the pod.

Typical flow:

1. A pod manifest references a macOS VM image in the custom OCI format.
2. The kubelet pulls the image from the registry (cached locally, see [Imaging](#imaging)).
3. The VM boots; any side-car containers start alongside it.
4. The kubelet reports pod status, IPs, and lifecycle back to the control plane.

## Requirements

- Apple Silicon Mac (M-series). The binary builds for `darwin/arm64` only.
- A macOS host with Virtualization.framework support.
- A Kubernetes cluster to join the node to, plus a client certificate and key the kubelet uses to
  authenticate to the API server.
- A macOS VM base image in the custom OCI format, pushed to an OCI registry (see [Imaging](#imaging)).
- SSH enabled inside the VM image. `kubectl exec`/`attach` into the VM, the readiness probe, postStart hooks,
  and stats all run over SSH.
- A Docker daemon on the host (for example Colima) if you run side-car containers.
- Code signing with Virtualization.framework entitlements. Ad-hoc signing is enough for NAT; bridged
  networking needs Apple-approved vmnet entitlements (see [Networking](#networking)).

## Getting started

### Build and sign

Tooling versions are pinned in `.mise.toml` (Go, goreleaser, Ruby). Builds run on macOS and produce a
`MacOSVK.app` bundle.

```shell
make snapshot   # local development build
make release    # signed release build (set RELEASE_CERTIFICATE_NAME, RELEASE_PROVISION_PROFILE_PATH)
```

The binary needs Virtualization.framework entitlements. To ad-hoc sign a locally built binary:

```shell
codesign --entitlements resources/vz.entitlements -s - <YOUR BINARY PATH>
```

Bridged networking requires the additional vmnet entitlements in `resources/release.entitlements`, which need
Apple approval. See [Networking](#networking).

### Run a workload

1. **Create a base VM image.** Use a Virtualization.framework-compatible tool such as
   [macosvm](https://github.com/s-u/macosvm).
2. **Package and push it** in the custom OCI format with
   [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz):

   ```shell
   oras-macos-vz push -h
   ```

3. **Write a pod manifest** that references the OCI image. See [Examples](#examples).
4. **Run the pod.** The kubelet picks it up, pulls the image, and boots the VM. Status is reported back as for
   a regular container.
5. **Interact** with `kubectl exec` (and `attach`) into the VM or any side-car container.

Configure the kubelet with the [flags](#command-line-flags) and [environment variables](#environment-variables)
below.

## Networking

Networking runs in one of two modes.

### NAT (default)

- The VM gets a local IP via NAT.
- Fits most cases where external reachability is unnecessary.
- `kubectl exec`/`attach` reach the VM over SSH on its local IP. The VM IP is discovered from the host ARP
  table.

### Bridged

- Gives the VM a routable IP by attaching it to a host network interface (typically a tagged VLAN with DHCP).
- Each VM interface gets a generated MAC so its lease can be tracked.
- The kubelet discovers the VM IP by sniffing for that MAC with libpcap (tcpdump-style capture) and reports it
  to Kubernetes.

Enable bridged mode by setting `VZ_BRIDGE_INTERFACE` to the host interface name. Bridged mode requires the
VMNet and VM Networking capabilities, which need Apple approval; see this
[Apple Developer Forums thread](https://developer.apple.com/forums/thread/656411) for how to request them.
After approval, generate a Mac Development certificate, an App ID with those capabilities, and a provisioning
profile, then build a release binary signed with `resources/release.entitlements`.

## Imaging

VM images use a custom, OCI-compliant format. See the [OCI manifest example](example/oci_manifest.json).

### Compression

macOS images are large (tens of gigabytes with Xcode and simulators preinstalled). The packaging step
compresses image layers (parallel gzip) so they store and transfer at a fraction of the raw size, and the
kubelet decompresses on pull.

### Copy-on-write overlays

A host runs at most two VMs at once (`MaxVirtualMachines`, enforced by a semaphore; a third pod waits for a
free slot). This reflects a Virtualization.framework limit. To let concurrent VMs share one base image while
keeping independent state, each pod gets a copy-on-write
[clone](https://github.com/apple/darwin-xnu/blob/main/bsd/sys/clonefile.h) of the disk and auxiliary storage.
Overlays are removed when the VM stops; the base image is never mutated.

### Digest validation

Local image files carry a recorded digest to guarantee integrity:

- On first download, a digest is computed and stored alongside the image file.
- On every pod start, the digest is checked against the expected digest from the registry manifest.
- If the digest file is missing or is not newer than the image file, the digest is recomputed from the image;
  a mismatch invalidates the local cache and the image is re-pulled.

## Building an image from a cached one

`MacOSVK build-image` (run as the provider's user, never a Pod feature) derives a new image from one the provider has already cached, without downloading it again:

```
MacOSVK build-image --from <registry/ns/name:tag> --to <registry/ns/name:tag> \
  --script /abs/path/script.sh --registry-creds-file /abs/path/creds.json
```

- `--from` and `--to` must both start with an entry of `VZ_ALLOWED_IMAGE_PREFIXES` (required for this command), and `--from` must be fully cached and digest-verified.
- The disk and aux images are APFS-cloned into `<cache>/build/` (same volume), booted headless, the script runs in the guest as `VZ_SSH_USER` over the existing key, then the VM is stopped from the host (a guest shutdown leaks a VM slot on macOS 26.5).
- The result is streamed to the registry as gzip layers with no temp file: each layer is read twice (digest first, then upload). `creds.json` is `{"username":"..","password":".."}` or `{"identityToken":".."}`, owned by the user running the build, mode 0600. The script must be owned by that user and not group/other writable.
- `--data-disk-gib N` builds a data disk instead: a blank sparse raw disk of N GiB (at most 256) is attached to the build VM as a second writable device, the script finds it and formats it (for example `diskutil eraseDisk APFS Xcode GPT diskN`), and only that disk is pushed as a one-layer image. The recipe must leave the data disk unmountable cleanly (no open files, no process using it): before the host stop the build unmounts it over SSH (`sudo -n diskutil unmountDisk force`, on the one physical disk of exactly N GiB) and fails if that does not work. The boot clone is discarded. Size N tightly: the disk is filled, so the free-space check requires `--expected-growth-gib` plus N GiB, and the disk guard reserves the full N GiB on every node that pulls the image. Pods attach the result with the `dynacom.dev/data-disks` annotation (see Storage).
- It refuses to run while a macOS virtual machine owned by root or by the user running the build (the provider's) is running, or one owned by a user named with `--count-vm-user` (repeatable); Linux VMs such as Colima's are not counted, and a failure to enumerate refuses the run. It honours `VZ_MIN_FREE_BYTES` throughout, and always deletes the clone. Peak extra disk is the blocks the script changes (`--expected-growth-gib`, default 10).

## Importing a Tart image

`MacOSVK import-tart --local` converts a public Tart disk-v2 image directly into
the provider's local cache, without an Apple ID or a running VM. Use `--local` for
this workflow: it never contacts or uploads to the destination registry. `--to`
still names the reference used by `build-image --from` and Pods.

```sh
MacOSVK import-tart --local \
  --src ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:71d9dc1d6c4614b7ecbb328753124912b43425fc8cf1c4085d7f352026df6601 \
  --to registry.s.dynacom.dev/macos-vz/tahoe-xcode:27 \
  --cache-dir /abs/path/provider-cache
```

Run the import as the **same user as the provider** (root, or the user the provider runs as) and
use the same `--cache-dir` for the import and build. The default is that user's
OS cache directory plus the provider's app identifier. A different user's cache
will not be found, and `build-image` requires cached inputs owned by the user running it.

The source is anonymous by default; `--src-creds` authenticates a private source.
Omitting `--local` retains registry publishing and requires a later pull before
building; `--creds` authenticates only that destination. Credential files use the
registry schema above, must belong to the invoking user and have mode 0600.
No destination credentials are needed for a local import.

If the import does not fit above `VZ_MIN_FREE_BYTES`, it fails with
`insufficient free disk space`. Pass `--evict` to let it delete least-recently-used
cached images under `<cache-dir>/blobs` first, using the downloader's eviction logic,
and fail only if it still does not fit. Local-only images (those with a
`.local-manifest.json`), the destination reference, every reference passed with the
repeatable `--keep <ref>` flag, and any image used (pulled or started) in the last 24 hours
are never evicted. If the remaining candidates cannot free enough space, the import fails
before evicting anything. Each eviction is logged with its reference directory and size. Entries are evicted
one at a time and free space is re-read after each, but APFS clones or snapshots can
keep deleted blocks allocated, so evictions may free less than expected. **Pass `--keep` for every image a
long-running Pod uses**: the one-shot importer cannot see which cached images
running VMs use (the 24 hour window only covers recent starts), and VM disks are APFS clones
with no link back to the cache entry, so no reliable in-use signal exists.

The importer shares the build lock, estimates storage from the uncompressed sizes of
non-zero chunks plus auxiliary storage, a small metadata margin, 1 GiB and
`VZ_MIN_FREE_BYTES`, and removes temporary
files on success or failure. Downloads and decompression are streamed with digest
checks; zero blocks remain sparse. Each blob has up to five download attempts
with backoff and ranged resume when supported by the source. The pinned image has a 140,000,000,000-byte raw
disk. Local imports hash the same gzip representation as a push, then move the
unpacked files on the same volume, without storing a compressed copy or copying
the full disk again. Existing destination cache directories are refused; choose
a fresh tag for another import.

The local cache contains `disk.img`, `aux.img`, `config.json` and their verified
`.digest` sidecars in the exact downloader reference directory under `blobs/`.
A `.local-manifest.json` and its digest preserve the manifest, including the OCI
config descriptor and layer descriptors. `build-image --from` automatically uses
this manifest offline, so no extra offline flag or preliminary Pod pull is needed.
The provider also uses it without registry access with `imagePullPolicy:
IfNotPresent` (set that explicitly, especially for `latest`). Missing layers or
invalid local metadata fail without registry fallback. `Always` explicitly pulls
from the registry and clears the local marker after a successful pull.

Guest access is unchanged by import. This Cirrus image uses guest user `admin`
and password `admin`. Run the build with `VZ_SSH_USER=admin` and
`VZ_SSH_PASSWORD=admin` in the **root build process's environment**, or use
`VZ_SSH_PRIVATE_KEY_PATH` / `VZ_SSH_PRIVATE_KEY_BASE64` only after the matching
public key is installed in the guest. Unset unrelated key variables when using
the password, since an invalid configured key fails before password authentication.
`build-image` does **not** read credentials JSON from stdin and does **not** accept
`guest_password`: that field is for `create-base`. Its required
`--registry-creds-file` contains only destination registry authentication.

Run the recipe using `build-image --from` with the imported reference, a different
`--to`, the same cache directory, `--script` and `--registry-creds-file`; both
references must be allowed by `VZ_ALLOWED_IMAGE_PREFIXES`. For the Xcode data-disk
recipe also pass `--data-disk-gib N`. The script runs as `admin` and any `sudo -n`
commands require passwordless sudo in the guest. Booting requires compatible
Apple Silicon hardware and host macOS. **The build result is still pushed**:
local import only avoids uploading the large base image. Plan space for the
recipe's result before running the build against a full registry.

## Creating a base image from an IPSW

`MacOSVK create-base` (run as the provider's user, never a Pod feature) installs macOS from an Apple IPSW and pushes the result as a base image, with no other tooling:

```
MacOSVK create-base --ipsw-url https://... --ipsw-sha256 <hex> \
  --to registry.s.dynacom.dev/macos-vz/<name>:<tag> \
  --script /abs/path/script.sh --creds /abs/path/creds.json [--cpu N --memory-gib N --disk-gib N]
```

- `--to` must be under `registry.s.dynacom.dev/macos-vz/`. The IPSW is downloaded over HTTPS into `<cache>/create-base/`, its SHA-256 is verified (a mismatch fails and deletes the download) and a verified cached copy is reused.
- macOS is installed with `VZMacOSInstaller` using the restore image's most featureful supported configuration. Setup Assistant is skipped by seeding the guest's data volume from the host (user `admin`, `.AppleSetupDone`, sshd on, `authorized_keys` for the `VZ_SSH_PRIVATE_KEY_*` key; `VZ_SSH_USER` must be `admin`). The same seed adds `admin` to the admin group and writes the passwordless sudoers.d entry and a key-only sshd drop-in, so the password never crosses SSH. On first boot (IP lookup budget 10 minutes) these are only verified, and FileVault is checked to be off. Then the script runs, the VM is stopped from the host and the image is pushed.
- `creds.json` is `{"username":"..","password":"..","guest_password":".."}` (`identityToken` may replace username and password), owned by the user running it, mode 0600. The guest password is never printed.
- It refuses to run while a macOS virtual machine owned by root, by the user running it, or by a `--count-vm-user` user, is running.
- `--then-script <file>` (optional): after `--script` the host stops the VM, boots it again and runs a second script (same file rules). For recipes that change the boot volume and need a reboot to finish.
- `--pre-recovery-script <file>` (needs `--recovery-sip-off`): before Recovery the seeded guest boots normally, is verified, runs the script as `admin` (bash, the guest password is the first line of its stdin and is never put on argv or logged) and is stopped. Use it for what Recovery needs first, such as a SecureToken for `admin` so `csrutil` has an authorized user.
- `--post-script-recovery-script <file>` (needs `--then-script`): after the first script the host stops the VM, starts it in macOS Recovery a second time and plays this script (same format and screenshots as `--recovery-script`), then boots it for the second script. For work only Recovery can do once the first script has blessed a new root snapshot, such as deleting the old one.
- `--recovery-sip-off` (opt-in, for CI guests whose owner accepts SIP off): after seeding, the guest starts once in macOS Recovery (`startUpFromMacOSRecovery`) and a script that looks at the screen (boot picker detection, wait-for-change loops, pointer clicks) and types runs `csrutil disable` and `csrutil authenticated-root disable`; keys go in through the private `_VZVNCServer` on a random-password port and a minimal RFB client. `--recovery-script <file>` replaces the built-in script (verbs `note`, `wait`, `wait-visible`, `wait-leave-picker`, `wait-change`, `wait-stable`, `click`, `key`, `type`, `secret guest_password`; see `internal/recoverykeys`). After the normal first boot the build fails unless `csrutil status` and `csrutil authenticated-root status` both report disabled. During the Recovery step a PNG screenshot is saved (mode 0600) after connecting, after every wait, key, type and click step, and before stopping, into `<--recovery-shots-dir or <cache-dir>/recovery-shots>/<UTC timestamp>/`; each path is logged and the files are kept. Untested on hardware.

## Feature overview

The tables below list supported Kubernetes features. Anything not listed is unsupported.

### Node

| Feature                  | Supported | Comments          |
|--------------------------|:---------:|-------------------|
| Node addresses           | ✅        |                   |
| Node capacity            | ✅        |                   |
| Node daemon endpoints    | ✅        |                   |
| Operating system         | ✅        | Darwin macOS only |

### Pod

| Feature                          | Supported | Comments                                                                                              |
|----------------------------------|:---------:|------------------------------------------------------------------------------------------------------|
| Create and delete pods           | ✅        |                                                                                                      |
| Update pods                      | ❌        | Recreate the pod instead.                                                                             |
| Get pod, pods, and pod status    | ✅        |                                                                                                      |
| Security policies                | ❌        |                                                                                                      |
| Init containers                  | ❌        | On the short list.                                                                                   |
| Regular containers               | ✅        | Container 0 must be the macOS VM; every later container runs on the Docker daemon.                    |

### Containers

| Feature                              | Supported | Comments                                                                                                                          |
|--------------------------------------|:---------:|----------------------------------------------------------------------------------------------------------------------------------|
| Container logs                       | ⚠️        | Docker side-cars only.                                                                                                            |
| Container exec                       | ✅        | VM exec needs `VZ_SSH_USER` plus one of `VZ_SSH_PRIVATE_KEY_BASE64` / `VZ_SSH_PRIVATE_KEY_PATH` / `VZ_SSH_PASSWORD`. Side-car exec works by default. |
| Container attach                     | ✅        | Docker attach; macOS VM via an interactive SSH shell.                                                                             |
| Container metrics                    | ✅        | Both backends: macOS VM (CPU and memory) and Docker side-cars.                                                                    |
| Resource requests                    | ⚠️        | Size the macOS VM. Docker containers ignore them.                                                                                 |
| Resource limits                      | ❌        | Ignored; VMs are fixed-size.                                                                                                      |
| Liveness / readiness / startup probes | ❌       | Pod-spec probes are not evaluated. See the readiness note below.                                                                  |
| Lifecycle hooks (postStart, preStop) | ⚠️        | Exec-shaped hooks only. A successful postStart hook gates pod Ready; preStop runs within the grace period.                        |

**Readiness gating.** Pod-spec probes are not evaluated, but every macOS pod is held NotReady until an internal
SSH readiness check succeeds (the guest sshd must answer), bounded by `VZ_SSH_READINESS_TIMEOUT`. If a pod
defines an exec postStart hook, Ready is gated on that hook finishing successfully (bounded by
`VZ_POSTSTART_TIMEOUT`); a permanently unreachable VM fails the pod rather than hanging.

### Storage

| Feature                  | Supported | Comments                                   |
|--------------------------|:---------:|--------------------------------------------|
| Host volumes             | ⚠️        | Denied unless allowed by `VZ_HOSTPATH_ALLOWLIST`. |
| Data disks               | ✅        | Read-only images attached as block devices. See below. |
| Empty dir volumes        | ✅        | `sizeLimit` is not enforced.               |
| Persistent volumes       | ❌        | Unsupported by Virtual Kubelet in general. |
| Config map volumes       | ❌        | On the short list.                         |
| Secret volumes           | ❌        | On the short list.                         |
| Projected volumes        | ⚠️        | See below.                                 |

**Data disks.** The pod annotation `dynacom.dev/data-disks: "<ref>,<ref>"` attaches up to 4 disk images to the macOS VM as
read-only virtio block devices, after the boot disk and in the listed order. Each reference must be pullable and start with an entry of
`VZ_ALLOWED_IMAGE_PREFIXES`, with no empty entry and no duplicate, and the annotation is refused on a `dynacom.dev/linux-only` pod; an
invalid annotation fails the create with an invalid-input error. The images are pulled in parallel with the VM image using the pod's
registry credentials (`imagePullPolicy` does not apply: a cached copy with a matching digest is used), cached like any image and never
evicted while a pod uses them. Two VMs share one cached file read-only. A data disk is built with `build-image --data-disk-gib`
and cannot be booted: using one as the pod image fails with "image is a data disk, not a bootable VM image".

A [projected volume](https://kubernetes.io/docs/concepts/storage/projected-volumes) maps several volume sources
into one directory. Kubernetes adds one by default carrying the service account token, API server CA, and
namespace, so a pod can call the API server.

| Source                | Supported | Comments                                                       |
|-----------------------|:---------:|----------------------------------------------------------------|
| configMap             | ✅        |                                                                |
| serviceAccountToken   | ⚠️        | No rotation. Expires per Kubernetes default (~3607s).          |
| downwardAPI           | ⚠️        | `metadata.namespace` only.                                     |
| secret                | ❌        | Projected secret sources are not mounted.                      |
| clusterTrustBundle    | ❌        |                                                                |

### Image pull secrets

The kubelet resolves registry credentials from the pod's `spec.imagePullSecrets` and from the pod
ServiceAccount's `imagePullSecrets` in the same namespace.

Supported secret types:

- `kubernetes.io/dockerconfigjson` (preferred), requires `.dockerconfigjson` data.
- `kubernetes.io/dockercfg` (legacy), requires `.dockercfg` data.

Secrets of other types are ignored with a warning event. A referenced secret that is missing or invalid fails
pod creation.

## Configuration

### Command-line flags

All flags are optional. Duration flags take Go duration strings (for example `30s`, `1m`).

| Flag                                            | Type     | Default                          | Description                                              |
|-------------------------------------------------|----------|----------------------------------|----------------------------------------------------------|
| `--nodename`                                    | string   | host name                        | Node name in the cluster.                                |
| `--provider-id`                                 | string   | (empty)                          | Provider ID reported to the API server.                  |
| `--startup-timeout`                             | duration | `0`                              | How long to wait for the kubelet to start.               |
| `--disable-taint`                               | bool     | `false`                          | Disable the node taint.                                  |
| `--log-level`                                   | string   | `info`                           | Log level.                                               |
| `--pod-sync-workers`                            | int      | `10`                             | Number of pod synchronization workers.                   |
| `--full-resync-period`                          | duration | `1m`                             | Interval between full pod resyncs.                        |
| `--client-verify-ca`                            | string   | `$APISERVER_CA_CERT_LOCATION`    | CA cert used to verify client requests.                  |
| `--no-verify-clients`                           | bool     | `false`                          | Do not require client certificate validation.            |
| `--authentication-token-webhook`                | bool     | `false`                          | Use the TokenReview API for bearer-token authentication. |
| `--authentication-token-webhook-cache-ttl`      | duration | `0`                              | Cache TTL for webhook token authentication responses.    |
| `--authorization-webhook-cache-authorized-ttl`  | duration | `0`                              | Cache TTL for authorized webhook responses.              |
| `--authorization-webhook-cache-unauthorized-ttl`| duration | `0`                              | Cache TTL for unauthorized webhook responses.            |
| `--trace-sample-rate`                           | string   | always sample                    | Trace sampling probability.                              |

Standard klog logging flags are also available under a `--klog.` prefix (for example `--klog.v`).

### Environment variables

#### Kubernetes connection

| Variable                     | Required | Default          | Description                                                       |
|------------------------------|:--------:|------------------|-------------------------------------------------------------------|
| `KUBECONFIG`                 |          | `~/.kube/config` | Path to the kubeconfig.                                            |
| `APISERVER_CERT_LOCATION`    | ✓        |                  | Client cert for authenticating to the API server.                 |
| `APISERVER_KEY_LOCATION`     | ✓        |                  | Client key for authenticating to the API server.                  |
| `APISERVER_CA_CERT_LOCATION` |          |                  | CA cert to verify the API server (default for `--client-verify-ca`). |
| `KUBELET_PORT`               |          | `10250`          | Kubelet API listen port.                                          |
| `VKUBELET_POD_IP`            |          |                  | Pod IP for the kubelet. For debugging.                            |
| `VKUBELET_TAINT_KEY`         |          | `virtual-kubelet.io/provider` | Node taint key.                                      |
| `VKUBELET_TAINT_EFFECT`      |          | `NoSchedule`     | Node taint effect.                                                |
| `VKUBELET_TAINT_VALUE`       |          | `macos-vz`       | Node taint value.                                                 |

#### macOS VM access (SSH)

Used for `kubectl exec`/`attach` into the VM, the readiness probe, postStart hooks, stats, and graceful
shutdown. Provide one of the key variables or `VZ_SSH_PASSWORD`.

| Variable                        | Required | Default           | Description                                                                 |
|---------------------------------|:--------:|-------------------|-----------------------------------------------------------------------------|
| `VZ_SSH_USER`                   | ✓        |                   | SSH username on the macOS VM.                                               |
| `VZ_SSH_PRIVATE_KEY_BASE64`     |          |                   | Base64-encoded PEM private key. Preferred when set.                         |
| `VZ_SSH_PRIVATE_KEY_PATH`       |          |                   | Private key file path. Used when `VZ_SSH_PRIVATE_KEY_BASE64` is unset.      |
| `VZ_SSH_PRIVATE_KEY_PASSPHRASE` |          |                   | Passphrase for the private key.                                            |
| `VZ_SSH_PASSWORD`               |          |                   | Password authentication (fallback when no key is set).                      |
| `VZ_SSH_KEX_ALGORITHMS`         |          | Go SSH defaults   | Comma-separated KEX algorithm override (advanced troubleshooting).          |
| `VZ_SSH_DIAL_TIMEOUT`           |          | `5s`              | Cap on TCP connect plus SSH handshake. Keeps `exec`/`attach` off an unreachable VM. |
| `VZ_MIN_FREE_BYTES`            |          | `16106127360` (15 GiB) | Free-space floor on the image cache volume. A pull that would leave less is refused (after evicting unused cached images, least recently used first); a pull that drops below it is aborted. `0` disables. |
| `VZ_SSH_READINESS_TIMEOUT`      |          | `60s`             | Overall cap on the post-start SSH readiness loop. On expiry the pod fails.  |
| `VZ_POSTSTART_TIMEOUT`          |          | `10s`             | Timeout for the postStart hook exec only.                                   |
| `TERM`                          |          | `xterm-256color`  | Terminal type for interactive `exec`/`attach`.                             |

#### Networking and side-cars

| Variable              | Required | Default                | Description                                                              |
|-----------------------|:--------:|------------------------|--------------------------------------------------------------------------|
| `VZ_BRIDGE_INTERFACE` |          | (empty, NAT)           | Host interface for bridged networking. Requires vmnet entitlements.      |
| `DOCKER_HOST`         |          | Docker SDK auto-detect | Docker daemon address for side-car containers.                           |

#### Host path volumes

| Variable                | Required | Default      | Description                                                                 |
|-------------------------|:--------:|--------------|-----------------------------------------------------------------------------|
| `VZ_HOSTPATH_ALLOWLIST` |          | (empty, deny)| Comma-separated absolute path prefixes that Pod `hostPath` volumes may use. Symlinks below the prefix are refused and re-checked just before the VM or Docker opens the path; a swap in that last instant cannot be excluded. |

The provider can run as root, so `hostPath` is deny-by-default. Each requested path is cleaned, `..` is
refused, symlinks are resolved on the deepest existing ancestor, and the result must sit under an allowed
prefix on a path boundary (`/a` does not allow `/ab`). The directory is only created once allowed. A denied
volume fails Pod creation with the reason in the Pod status. `volumeMount.readOnly` is honoured for both
Docker side-cars and macOS VM shared directories.

#### Pod volume directory

| Variable             | Required | Default                  | Description                                                              |
|----------------------|:--------:|--------------------------|--------------------------------------------------------------------------|
| `VZ_POD_VOLUMES_DIR` |          | `<cache dir>/mounts`     | Absolute directory where `emptyDir`, `configMap`, `secret`, `projected` and downward API volumes are written, for Docker side-cars and the macOS VM. |

Set it to a directory your Docker VM (for example Colima) shares, such as a virtiofs mount, so bind sources
are visible to the containers. At start the provider deletes only the children of this directory, never the
directory itself, so a share pointing at it survives. A missing directory is created with mode 0755; an existing
one keeps its mode, ownership and ACLs. A relative path or a symlink is refused and the provider does not start.
Per-pod subdirectories and files are created without changing ACLs, so ACEs inherited from the parent apply.
`emptyDir` directories are set to mode 0777 so the container user can write to them (chmod does not remove
macOS ACL entries). `configMap` and `secret` volumes honour `items`, `defaultMode` (default 0644) and `optional`;
secrets are fetched by name, which the Node authorizer permits.

#### Tracing (OpenTelemetry)

| Variable                      | Required | Default          | Description                                |
|-------------------------------|:--------:|------------------|--------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` |          | tracing disabled | OTLP trace endpoint.                       |
| `OTEL_EXPORTER_OTLP_INSECURE` |          | `false`          | Disable TLS when exporting traces.         |
| `OTEL_SERVICE_NAME`           |          |                  | Service name reported with traces.         |

### Local cache

The cache directory is `~/Library/Caches/com.agoda.fleet.virtualization`. It holds OCI images and their digest
files, plus pod mount volumes when `emptyDir` volumes are used.

## Examples

The [example](example) directory has ready-to-adapt manifests:

- [`pod.yml`](example/pod.yml) - a minimal macOS VM pod.
- [`pod-gitlab-sidecar.yml`](example/pod-gitlab-sidecar.yml) - a hybrid pod with a macOS VM plus a GitLab
  Runner Docker side-car.
- [`deployment.yml`](example/deployment.yml) - a Deployment of macOS VM pods.
- [`oci_manifest.json`](example/oci_manifest.json) and [`config.json`](example/config.json) - the custom OCI
  image format.

## Roadmap

- Higher test coverage, including an open end-to-end testing approach.
- A public CI/CD pipeline to make contribution easier.

## Contributing

Issues and contributions are welcome on the
[GitHub Issues](https://github.com/agoda-com/macOS-vz-kubelet/issues) page. See [CONTRIBUTING.md](CONTRIBUTING.md)
for development and pull request guidance.

## Related projects

These projects are the foundation `macOS-vz-kubelet` is built on:

- [Virtual Kubelet](https://github.com/virtual-kubelet/virtual-kubelet) - the Kubernetes kubelet
  implementation this provider plugs into.
- [Code-Hex/vz](https://github.com/Code-Hex/vz) - Go bindings for Apple Virtualization.framework.
- [oras-project/oras-go](https://github.com/oras-project/oras-go) - ORAS Go library for OCI artifacts.
- [oras-macos-vz](https://github.com/agoda-com/oras-macos-vz) - the ORAS CLI fork used to package and push VM
  images.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
