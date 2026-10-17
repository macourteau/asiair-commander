# The SMB1 sidecar: Debian's Samba retains the deprecated SMB1/NT1 protocol
# support the ASIAIR-mimicking share needs (server min protocol = NT1), which
# the in-memory device fake cannot exercise.
# A Debian point-release tag rather than the bookworm-slim codename, so Dependabot
# has a version to bump.
FROM debian:12.15-slim
RUN apt-get update && apt-get install -y --no-install-recommends samba && rm -rf /var/lib/apt/lists/*
COPY smb.conf /etc/samba/smb.conf

# Seed a fake-ASIAIR EMMC tree: one real (PII-stripped) ASIAIR FITS light frame
# under the Autorun/Light/<target>/ layout the camera writes, its JPEG preview
# sidecar, and an exFAT recovery-junk file at the volume root — enough to
# exercise recursive listing, sidecar pairing, byte-exact reads, and junk
# skipping over the wire. seed/ is populated by run.sh from
# internal/header/testdata before the build (see .gitignore).
RUN mkdir -p "/shares/EMMC Images/Autorun/Light/NGC7000"
# JSON-array (exec) form: the destination path contains a space, which the
# shell form of COPY cannot quote.
COPY ["seed/asiair_light_2600mm_ha.fit", "/shares/EMMC Images/Autorun/Light/NGC7000/Light_NGC7000_001.fit"]
RUN printf 'JPEGPREVIEW' > "/shares/EMMC Images/Autorun/Light/NGC7000/Light_NGC7000_001_thn.jpg" && \
    printf 'exfat recovery junk' > "/shares/EMMC Images/FSCK0000.REC"

# A directory big enough that one night's TRANS2 FIND response cannot fit in a
# single SMB1 message. A server answers with as many bytes as its own send
# buffer holds (Samba's max_xmit, ~16.5 KiB) and continues in further messages
# under the same MID; a client that reads only the first message loses the tail
# of every batch SILENTLY, because the search still reports more to come. That
# defect would make a rig report "safe to power off" while un-pulled frames sat
# on the device (docs/design/2026-10-17-delete-safety.md §5).
#
# The names are full-length ASIAIR captures on purpose: at ~78 characters they
# encode to ~250 bytes per entry, so ~66 entries fill a message and every batch
# of 100 must fragment. The count is asserted exactly by the Go test.
RUN mkdir -p "/shares/EMMC Images/Autorun/Light/BIGNIGHT" && \
    for i in $(seq 1 400); do \
      n=$(printf '%04d' "$i"); \
      printf 'FRAME' > "/shares/EMMC Images/Autorun/Light/BIGNIGHT/Light_NGC7000_45deg_300.0s_Bin1_2600MM_H_gain100_20260101-220000_-10.0C_${n}.fit"; \
    done

# An empty directory of its own for the sliced-drain exact-count test
# (docs/design/2026-10-17-frame-lifecycle.md §12): SMB1 Create cannot make a
# missing parent directory, and the test's own frames must not share NGC7000's
# or BIGNIGHT's directory, or its delete pass would sweep up their fixtures too.
RUN mkdir -p "/shares/EMMC Images/Autorun/Light/DPYDRAIN"

# One empty directory per real-tunnel test that stages frames
# (remote_tunnel_test.go), for the same reason: each rig's delete pass must see
# only its own frames.
RUN for d in REMOTECYCLE REMOTEDROP REMOTERESTART REMOTESPARE; do \
      mkdir -p "/shares/EMMC Images/Autorun/Light/$d"; \
    done

# No account named "guest" is created on purpose. smb1client does a genuine
# NTLMv2 handshake, and the ASIAIR has no such account to match it against: it
# answers by granting a guest session instead (smb.conf's map to guest = bad
# user). The client must accept that downgrade — see TestServerGrantsGuestLogon,
# which asserts the fixture still behaves this way.
RUN chown -R nobody "/shares"
EXPOSE 445
CMD ["smbd", "--foreground", "--no-process-group", "--debug-stdout"]
