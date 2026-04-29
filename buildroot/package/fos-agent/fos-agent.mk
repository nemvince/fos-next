################################################################################
#
# fos-agent
#
# Builds the static fos-agent Go binary and installs it into the sysroot.
# The binary is linked with CGO_ENABLED=0 so it has no shared library deps.
#
################################################################################

FOS_AGENT_VERSION = $(call qstrip,$(BR2_PACKAGE_FOS_AGENT_VERSION))
FOS_AGENT_SITE    = $(call qstrip,$(BR2_PACKAGE_FOS_AGENT_SITE))
FOS_AGENT_SITE_METHOD = local

# Always rebuild from the live source tree.
FOS_AGENT_OVERRIDE_SRCDIR = $(call qstrip,$(BR2_PACKAGE_FOS_AGENT_SITE))

FOS_AGENT_LICENSE           = GPL-3.0
FOS_AGENT_LICENSE_FILES     = LICENSE
FOS_AGENT_DEPENDENCIES      = host-go

# Go cross-compilation uses the host Go toolchain with GOOS/GOARCH set.
FOS_AGENT_GOARCH      = amd64
FOS_AGENT_GOOS        = linux

# Version information injected at link time.
# FOS_VERSION / FOS_COMMIT / FOS_BUILD_DATE may be set by the CI environment.
# Fall back to git-derived values for local builds.
FOS_VERSION    ?= $(shell git -C $(BR2_EXTERNAL_FOS_NEXT_PATH) describe --tags --always 2>/dev/null || echo dev)
FOS_COMMIT     ?= $(shell git -C $(BR2_EXTERNAL_FOS_NEXT_PATH) rev-parse --short HEAD 2>/dev/null || echo unknown)
FOS_BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

FOS_AGENT_VERSION_PKG = github.com/nemvince/fos-next/internal/version
FOS_AGENT_LDFLAGS = -s -w \
	-X $(FOS_AGENT_VERSION_PKG).Version=$(FOS_VERSION) \
	-X $(FOS_AGENT_VERSION_PKG).Commit=$(FOS_COMMIT) \
	-X $(FOS_AGENT_VERSION_PKG).BuildDate=$(FOS_BUILD_DATE)

define FOS_AGENT_BUILD_CMDS
	cd $(FOS_AGENT_SRCDIR) && \
	CGO_ENABLED=0 GOOS=$(FOS_AGENT_GOOS) GOARCH=$(FOS_AGENT_GOARCH) \
	$(HOST_DIR)/bin/go build \
		-ldflags="$(FOS_AGENT_LDFLAGS)" \
		-o $(@D)/fos-agent \
		./cmd/fos-agent
endef

define FOS_AGENT_INSTALL_TARGET_CMDS
	$(INSTALL) -D -m 0755 $(@D)/fos-agent $(TARGET_DIR)/sbin/fos-agent
endef

$(eval $(generic-package))
