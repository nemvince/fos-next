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

FOS_AGENT_LICENSE           = GPL-3.0
FOS_AGENT_LICENSE_FILES     = LICENSE
FOS_AGENT_DEPENDENCIES      = host-go

# Go cross-compilation uses the host Go toolchain with GOOS/GOARCH set.
FOS_AGENT_GOARCH      = amd64
FOS_AGENT_GOOS        = linux

# Version information injected at link time.
# FOS_VERSION / FOS_COMMIT / FOS_BUILD_DATE are set by the CI environment
# (see .github/workflows/build.yml). Fall back to "dev" for local builds.
FOS_VERSION    ?= dev
FOS_COMMIT     ?= unknown
FOS_BUILD_DATE ?= unknown

FOS_AGENT_VERSION_PKG = github.com/nemvince/fos-next/internal/version
FOS_AGENT_LDFLAGS = -s -w \
	-X $(FOS_AGENT_VERSION_PKG).Version=$(FOS_VERSION) \
	-X $(FOS_AGENT_VERSION_PKG).Commit=$(FOS_COMMIT) \
	-X $(FOS_AGENT_VERSION_PKG).BuildDate=$(FOS_BUILD_DATE)

define FOS_AGENT_BUILD_CMDS
	cd $($(PKG)_SRCDIR) && \
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
