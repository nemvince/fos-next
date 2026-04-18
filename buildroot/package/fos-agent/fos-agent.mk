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

define FOS_AGENT_BUILD_CMDS
	cd $($(PKG)_SRCDIR) && \
	CGO_ENABLED=0 GOOS=$(FOS_AGENT_GOOS) GOARCH=$(FOS_AGENT_GOARCH) \
	$(HOST_DIR)/bin/go build \
		-ldflags="-s -w" \
		-o $(@D)/fos-agent \
		./cmd/fos-agent
endef

define FOS_AGENT_INSTALL_TARGET_CMDS
	$(INSTALL) -D -m 0755 $(@D)/fos-agent $(TARGET_DIR)/sbin/fos-agent
endef

$(eval $(generic-package))
