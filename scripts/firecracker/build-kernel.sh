#!/usr/bin/env bash
# Build a full-featured Firecracker guest kernel (uncompressed vmlinux) that boots
# a real server: Firecracker virtio essentials PLUS everything a container runtime
# and systemd need (overlayfs, bridge/veth, netfilter/iptables/nftables + NAT +
# conntrack, br_netfilter, all cgroup controllers, namespaces, autofs, tun, bpf).
#
# It compiles inside an ubuntu:22.04 container (the toolchain the Firecracker CI
# kernels are built with — gcc 11), so it is reproducible on any Docker host
# regardless of the host distro/compiler.
#
# Output (under $OUT, default ~/fc-assets):
#   vmlinux-full   — the guest kernel
set -euo pipefail

OUT="${OUT:-$HOME/fc-assets}"
KVER="${KVER:-6.1.102}"                 # matches the known-good CI kernel line
KMAJ="v6.x"
FC_CONFIG_URL="${FC_CONFIG_URL:-https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config}"
JOBS="${JOBS:-$(nproc)}"
mkdir -p "$OUT"

# Config fragment merged on top of the Firecracker guest config. These are the
# container-runtime + systemd requirements (Docker's check-config.sh set, plus the
# server niceties); most are additive to the microVM base.
cat > "$OUT/fc-full.fragment" <<'FRAG'
# --- namespaces & cgroups (containers) ---
CONFIG_NAMESPACES=y
CONFIG_UTS_NS=y
CONFIG_IPC_NS=y
CONFIG_PID_NS=y
CONFIG_NET_NS=y
CONFIG_USER_NS=y
CONFIG_CGROUPS=y
CONFIG_CGROUP_BPF=y
CONFIG_CGROUP_CPUACCT=y
CONFIG_CGROUP_DEVICE=y
CONFIG_CGROUP_FREEZER=y
CONFIG_CGROUP_PIDS=y
CONFIG_CGROUP_SCHED=y
CONFIG_CGROUP_HUGETLB=y
CONFIG_CPUSETS=y
CONFIG_MEMCG=y
CONFIG_BLK_CGROUP=y
CONFIG_CGROUP_WRITEBACK=y
CONFIG_CFS_BANDWIDTH=y
CONFIG_FAIR_GROUP_SCHED=y
# --- security / keys / seccomp ---
CONFIG_KEYS=y
CONFIG_SECCOMP=y
CONFIG_SECCOMP_FILTER=y
CONFIG_BPF_SYSCALL=y
# --- storage drivers ---
CONFIG_OVERLAY_FS=y
CONFIG_EXT4_FS=y
CONFIG_FUSE_FS=y
# --- container networking ---
CONFIG_BRIDGE=y
CONFIG_BRIDGE_NETFILTER=y
CONFIG_VETH=y
CONFIG_VXLAN=y
CONFIG_MACVLAN=y
CONFIG_IPVLAN=y
CONFIG_DUMMY=y
CONFIG_TUN=y
# --- netfilter core + nat + conntrack ---
CONFIG_NETFILTER=y
CONFIG_NETFILTER_ADVANCED=y
CONFIG_NF_CONNTRACK=y
CONFIG_NF_CONNTRACK_NETLINK=y
CONFIG_NF_NAT=y
CONFIG_NF_TABLES=y
CONFIG_NF_TABLES_INET=y
CONFIG_NFT_CT=y
CONFIG_NFT_NAT=y
CONFIG_NFT_MASQ=y
CONFIG_NFT_COMPAT=y
# --- iptables (legacy, still used by docker/kube) ---
CONFIG_IP_NF_IPTABLES=y
CONFIG_IP_NF_FILTER=y
CONFIG_IP_NF_NAT=y
CONFIG_IP_NF_TARGET_MASQUERADE=y
CONFIG_IP_NF_TARGET_REJECT=y
CONFIG_IP_NF_MANGLE=y
CONFIG_IP6_NF_IPTABLES=y
CONFIG_IP6_NF_FILTER=y
CONFIG_IP6_NF_NAT=y
CONFIG_NETFILTER_XT_MATCH_ADDRTYPE=y
CONFIG_NETFILTER_XT_MATCH_CONNTRACK=y
CONFIG_NETFILTER_XT_MATCH_IPVS=y
CONFIG_NETFILTER_XT_MARK=y
CONFIG_NETFILTER_XT_TARGET_MASQUERADE=y
CONFIG_IP_VS=y
CONFIG_IP_VS_NFCT=y
CONFIG_IP_VS_RR=y
# --- systemd requirements ---
CONFIG_FHANDLE=y
CONFIG_AUTOFS_FS=y
CONFIG_AUTOFS4_FS=y
CONFIG_FANOTIFY=y
CONFIG_INOTIFY_USER=y
CONFIG_SIGNALFD=y
CONFIG_TIMERFD=y
CONFIG_EPOLL=y
CONFIG_POSIX_MQUEUE=y
CONFIG_CROSS_MEMORY_ATTACH=y
CONFIG_TMPFS_POSIX_ACL=y
CONFIG_TMPFS_XATTR=y
# --- misc a real box may want ---
CONFIG_VETH=y
CONFIG_IKCONFIG=y
CONFIG_IKCONFIG_PROC=y
FRAG

docker run --rm -v "$OUT":/out ubuntu:22.04 bash -euc "
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq build-essential flex bison bc libelf-dev libssl-dev \
    wget xz-utils ca-certificates cpio kmod >/dev/null
  cd /tmp
  echo '>> downloading linux-${KVER}'
  wget -q https://cdn.kernel.org/pub/linux/kernel/${KMAJ}/linux-${KVER}.tar.xz
  tar xf linux-${KVER}.tar.xz
  cd linux-${KVER}
  echo '>> fetching Firecracker guest config base'
  wget -q -O .config '${FC_CONFIG_URL}'
  echo '>> merging full-server config fragment'
  ./scripts/kconfig/merge_config.sh -m .config /out/fc-full.fragment
  make olddefconfig
  echo '>> building vmlinux with ${JOBS} jobs (this takes a while)'
  make -j${JOBS} vmlinux
  cp vmlinux /out/vmlinux-full
  echo '>> built /out/vmlinux-full'
  ls -la /out/vmlinux-full
"
echo ">> done: $OUT/vmlinux-full"
