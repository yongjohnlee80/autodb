VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# -s -w drop the symbol table and DWARF: about 11 MB of a 36 MB binary. Panics
# still print full stack traces (the pclntab stays). Debug with `go test` or
# `dlv debug`, not this artefact.
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

# The dev checkout nests a worktree inside the bare store (family layout:
# autodb/.git bare + autodb/main worktree). Go's nested-VCS rule then points
# buildvcs at the bare dir and fails; version identity comes from LDFLAGS
# instead, so VCS stamping is disabled for all local targets. CI clones
# normally and is unaffected.
export GOFLAGS := -buildvcs=false

.PHONY: build build-gui test test-go test-lua vet clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/autodb ./cmd/autodb

# build-gui builds the windowed binary (golib/gui needs cgo and, on Linux,
# the Wayland/X11 headers — see golib/gui/README). The macOS release and the
# Linux `-gui` downloads are built this way; the plain Linux release is
# `build`'s shape, static, so it starts on a host with no window libraries.
# The dev `build` above stays cgo-less so a checkout with no window headers
# still builds the whole program.
# The Vulkan backend is optional: gio draws through EGL/OpenGL by default --
# Wayland tries EGL first unless the app asks for app.PreferVulkan, and X11 keeps
# Vulkan off entirely (gio's vulkanBuggy, gioui.org/issue/347) -- so a build
# without it is a complete GUI. What it costs is a compile-time header,
# vulkan/vulkan.h, which ships separately from the loader that installs vulkan.pc
# (Arch: vulkan-headers, Debian/Ubuntu: libvulkan-dev), so a host can satisfy
# `pkg-config --exists vulkan` and still fail to compile: build-gui probes the
# header itself.
#
#   VULKAN=auto  (default) with Vulkan when the header is there, without it when not
#   VULKAN=1     require it: fail naming the package when the header is missing
#   VULKAN=0     omit it: EGL/OpenGL only, no header needed
VULKAN ?= auto

build-gui:
	@tags=gui; \
	if printf '%s\n' '#include <vulkan/vulkan.h>' | $(CC) -E -x c - $$(pkg-config --cflags vulkan 2>/dev/null) >/dev/null 2>&1; \
		then have=yes; else have=no; fi; \
	case "$(VULKAN)" in \
	1) if [ $$have = no ]; then \
	     echo "make build-gui VULKAN=1: vulkan/vulkan.h not found (Arch: vulkan-headers, Debian/Ubuntu: libvulkan-dev)"; \
	     exit 1; \
	   fi ;; \
	0) tags=gui,novulkan ;; \
	*) if [ $$have = no ]; then \
	     tags=gui,novulkan; \
	     echo "make build-gui: no vulkan/vulkan.h -- building EGL/OpenGL only (VULKAN=1 to require Vulkan)"; \
	   fi ;; \
	esac; \
	set -x; CGO_ENABLED=1 go build -tags "$$tags" -trimpath -ldflags "$(LDFLAGS)" -o bin/autodb ./cmd/autodb

# `test` is the honest signal: BOTH sides. The Lua suite drives a real daemon and
# the neovim UI, and was previously in neither this target nor CI — so a whole
# class of behaviour (the handshake, the identity-refusal security boundary, the
# three-pane UI) had no automated signal at all. `test-go` stays for the Go-only
# path (no neovim needed).
test: test-go test-lua

test-go:
	go test ./...

# test-lua builds bin/autodb and runs the neovim smoke suite through a wrapper
# that fails on an aborted driver — nvim exits 0 on an uncaught Lua error, so the
# suite's own exit code cannot be trusted alone (see tests/run-all.sh -> run-smoke.sh).
test-lua:
	./tests/run-all.sh

vet:
	go vet ./...

clean:
	rm -rf bin dist
