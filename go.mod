module github.com/tunnel-pizza/tunneld

// 1.26 is libtunnel's own floor, and tunneld links it into the binary. Below
// that the module's own needs are older — 1.24 for generic type aliases (`type
// BuilderV1[T any] = v1.Builder[T]` in lib/lib.go) and slog.DiscardHandler —
// so this directive tracks the dependency, not the source. Raise it only when
// a dependency or the surface actually needs the newer semantics.
go 1.26.0

require (
	charm.land/bubbletea/v2 v2.0.9
	github.com/ProtonMail/go-crypto v0.0.0-20221026131551-cf6655e29de4
	github.com/cenkalti/backoff/v4 v4.1.3
	github.com/charmbracelet/colorprofile v0.4.3
	github.com/charmbracelet/ultraviolet v0.0.0-20260703014108-f5a850f9c2b7
	github.com/charmbracelet/x/ansi v0.11.7
	github.com/charmbracelet/x/vt v0.0.0-20260906004030-3986e9119cf9
	github.com/cnuss/libtunnel v0.3.0
	github.com/containerd/errdefs v1.0.0
	github.com/creack/pty v1.1.24
	github.com/dustin/go-humanize v1.0.1
	github.com/florianl/go-tc v0.4.5-0.20240822175159-7926c32f7299
	github.com/gliderlabs/ssh v0.3.5
	github.com/go-logr/logr v1.4.4
	github.com/google/go-tpm v0.9.2-0.20240919181259-d96ccf715685
	github.com/google/uuid v1.6.0
	github.com/gopacket/gopacket v1.2.0
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	github.com/insomniacslk/dhcp v0.0.0-20231206064809-8c70d406f6d2
	github.com/ishidawataru/sctp v0.0.0-20230406120618-7ff4192f6ff2
	github.com/jaypipes/ghw v0.12.0
	github.com/kevinburke/ssh_config v1.2.0
	github.com/mdlayher/vsock v1.2.1
	github.com/moby/moby/api v1.56.0
	github.com/moby/moby/client v0.6.0
	github.com/packetcap/go-pcap v0.0.0-20240528124601-8c87ecf5dbc5
	github.com/pkg/browser v0.0.0-20240102092130-5ac0b6a4141c
	github.com/pkg/sftp v1.13.9
	github.com/rck/unit v0.0.3
	github.com/sirupsen/logrus v1.9.3
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/spf13/viper v1.21.0
	github.com/u-root/cpuid v0.0.1-0.20250320140348-cc5fe81d966c
	github.com/u-root/gobusybox/src v0.0.0-20250101170133-2e884e4509c7
	github.com/u-root/iscsinl v0.1.1-0.20210528121423-84c32645822a
	github.com/u-root/u-root v0.16.0
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701
	github.com/vishvananda/netlink v1.3.0
	github.com/vishvananda/netns v0.0.4
	github.com/vtolstov/go-ioctl v0.0.0-20151206205506-6be9cced4810
	github.com/yuin/goldmark v1.8.6
	go.bug.st/serial v1.6.2
	golang.org/x/crypto v0.57.0
	golang.org/x/exp v0.0.0-20250305212735-054e65f0b394
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	golang.org/x/text v0.42.0
	golang.org/x/tools v0.49.0
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/cri-streaming v0.37.0
	k8s.io/klog/v2 v2.140.0
	pack.ag/tftp v1.0.1-0.20181129014014-07909dfbde3c
	rsc.io/qr v0.2.0
	src.elv.sh v0.21.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/StackExchange/wmi v1.2.1 // indirect
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be // indirect
	github.com/beevik/ntp v0.3.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/breml/rootcerts v0.3.7 // indirect
	github.com/bytedance/sonic/loader v0.5.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charmbracelet/x/exp/ordered v0.1.0 // indirect
	github.com/charmbracelet/x/term v0.2.2 // indirect
	github.com/charmbracelet/x/termios v0.1.1 // indirect
	github.com/charmbracelet/x/windows v0.2.2 // indirect
	github.com/clipperhouse/displaywidth v0.11.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/cloudflare/cloudflared v0.0.0-20260612062426-68620efbce4c // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/coreos/go-oidc/v3 v3.17.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/creack/goselect v0.1.2 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/go-connections v0.8.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/getsentry/sentry-go v0.43.0 // indirect
	github.com/ghodss/yaml v1.0.0 // indirect
	github.com/go-chi/chi/v5 v5.2.4 // indirect
	github.com/go-chi/cors v1.2.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.2.1 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jaypipes/pcidb v1.0.0 // indirect
	github.com/josharian/native v1.1.0 // indirect
	github.com/jsimonetti/rtnetlink v1.3.5 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/klauspost/pgzip v1.2.6 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/lucasb-eyer/go-colorful v1.4.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/mdlayher/netlink v1.7.2 // indirect
	github.com/mdlayher/packet v1.1.2 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/spdystream v0.5.1 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/muesli/cancelreader v0.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-dns v1.3.3 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/orangecms/go-framebuffer v0.0.0-20200613202404-a0700d90c330 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/prometheus/client_golang v1.24.1 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/quic-go/quic-go v0.62.0 // indirect
	github.com/rekby/gpt v0.0.0-20200219180433-a930afbc6edc // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/shirou/gopsutil/v4 v4.26.3 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/therootcompany/xz v1.0.1 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/ulikunitz/xz v0.5.15 // indirect
	github.com/urfave/cli/v2 v2.3.0 // indirect
	github.com/xo/terminfo v0.0.0-20220910002029-abceb7e1c41e // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0 // indirect
	go.opentelemetry.io/contrib/propagators v0.22.0 // indirect
	go.opentelemetry.io/otel v1.46.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.46.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260819154853-08b0e4226688 // indirect
	google.golang.org/grpc v1.83.2 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	howett.net/plist v1.0.0 // indirect
	k8s.io/streaming v0.37.0 // indirect
	k8s.io/utils v0.0.0-20260626114624-be93311217bd // indirect
	mvdan.cc/sh/v3 v3.11.0 // indirect
	nhooyr.io/websocket v1.8.7 // indirect
	zombiezen.com/go/capnproto2 v2.18.0+incompatible // indirect
)
