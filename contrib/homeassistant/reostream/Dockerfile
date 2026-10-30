# Copyright (c) 2026 Riley Poe. AGPL v3, see COPYRIGHT.

# The add-on is the published release image, unchanged but for how it is
# started. Home Assistant passes BUILD_VERSION, the version in config.yaml,
# which is also the image's tag.
#
# There is no run script, and nothing to run one with: the release image is
# distroless, with no shell. None is needed. The add-on has no options of
# its own -- cameras and everything else are set up on reostream's page,
# which Home Assistant opens through ingress -- so there is nothing in
# /data/options.json to translate into flags.
ARG BUILD_VERSION
FROM ghcr.io/volttech21/reostream:${BUILD_VERSION}

# Root, not the image's own uid 65532: the Supervisor mounts /data owned by
# root, and the first thing reostream does there is write its config.
USER root

# /data is the add-on's persistent directory, kept across updates and
# included in Home Assistant backups. -ha-ingress trusts the Supervisor's
# ingress proxy for the page's path prefix and for having signed the user
# in.
CMD ["-data", "/data", "-ha-ingress"]
