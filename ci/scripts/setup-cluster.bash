#!/bin/bash
# =============================================================================
# Stage 1 of the in-container flow: stand up a single-host WarehousePG demo
# cluster and build the gpbackup binaries, then leave the container running so
# the workflow can exec the test phases (`make integration`, `make end_to_end`)
# as separate, independently-reported steps.
#
# Writes /home/gpadmin/cluster-env.sh with everything the test phases need to
# source (greenplum_path + gpdemo-env + PATH).
#
# Env (from the container, set by the workflow's `docker run`):
#   GPHOME                    server install prefix, /usr/edb/whpg<major>.
#   NUM_PRIMARY_MIRROR_PAIRS  primary segment count (3 -> exercises the
#                             segmentCount==3-gated e2e specs).
# =============================================================================
set -euo pipefail

GPHOME="${GPHOME:-/usr/edb/whpg19}"
NUM_PRIMARY_MIRROR_PAIRS="${NUM_PRIMARY_MIRROR_PAIRS:-3}"
RESULTS_DIR="${HOME}/results"
mkdir -p "${RESULTS_DIR}"

echo "::group::Environment"
echo "GPHOME=${GPHOME}  NUM_PRIMARY_MIRROR_PAIRS=${NUM_PRIMARY_MIRROR_PAIRS}"
[ -f "${GPHOME}/greenplum_path.sh" ] || { echo "ERROR: ${GPHOME}/greenplum_path.sh not found — WHPG server not installed where expected" >&2; exit 1; }
echo "::endgroup::"

# --- sshd + passwordless ssh to self (gpinitsystem + e2e rsync -e ssh) ---
echo "::group::SSH setup"
# On WHPG6's el8 image, sshd's pam_nologin module rejects every non-root
# login with "System is booting up. Unprivileged users are not permitted to
# log in yet." -- /run/nologin (or /etc/nologin) is created as part of the
# base image / systemd package layout and is normally removed once systemd
# finishes booting, but nothing ever clears it in a plain container with no
# real init. This broke gpinitsystem's SSH-to-self segment setup (gpadmin,
# not root). Harmless to remove unconditionally; el9 (WHPG7/WHPG19) never
# has these files in the first place.
sudo rm -f /run/nologin /etc/nologin
sudo /usr/sbin/sshd
ssh-keygen -t rsa -N "" -f "${HOME}/.ssh/id_rsa" -q
cat "${HOME}/.ssh/id_rsa.pub" >> "${HOME}/.ssh/authorized_keys"
chmod 600 "${HOME}/.ssh/authorized_keys"
for h in localhost 127.0.0.1 "$(hostname)"; do
    ssh-keyscan -H "${h}" >> "${HOME}/.ssh/known_hosts" 2>/dev/null || true
done
chmod 700 "${HOME}/.ssh"
echo "::endgroup::"

# --- demo cluster: NUM_PRIMARY_MIRROR_PAIRS primaries, no mirrors ---
# Drive gpdemo through its Makefile, NOT demo_cluster.sh directly: the Makefile
# translates PORT_BASE -> DEMO_PORT_BASE (the var the script actually reads) and
# sets WITH_STANDBY / the postgresql addons file. Running the script bare leaves
# DEMO_PORT_BASE empty -> gpinitsystem fails with "COORDINATOR_PORT not set".
echo "::group::Create demo cluster"
# shellcheck disable=SC1091
source "${GPHOME}/greenplum_path.sh"
if ! make -C "${HOME}/gpdemo" create-demo-cluster \
    PORT_BASE=7000 \
    NUM_PRIMARY_MIRROR_PAIRS="${NUM_PRIMARY_MIRROR_PAIRS}" \
    WITH_MIRRORS=false 2>&1 | tee "${RESULTS_DIR}/demo_cluster.log"; then
    # gpinitsystem's own stdout (captured above) only shows its wrapper-level
    # summary, not the actual postgres server startup log -- pull both the
    # gpAdminLogs gpinitsystem log and the coordinator's own log/ directory
    # into results/ so a failure here is diagnosable from the uploaded
    # artifact instead of needing a live repro.
    echo "::group::Cluster init failure diagnostics"
    mkdir -p "${RESULTS_DIR}/cluster-failure-logs"
    cp "${HOME}/gpdemo/datadirs"/gpAdminLogs/gpinitsystem_*.log "${RESULTS_DIR}/cluster-failure-logs/" 2>/dev/null || true
    # pg_ctl's `-l logfile` is relative to wherever gpinitsystem invoked it
    # from, so the coordinator's startup log can be a plain "logfile" file
    # directly under its data directory, not necessarily under a log/
    # subdirectory -- cast a wide net.
    find "${HOME}/gpdemo/datadirs" -path '*qddir*' \( -name '*.log' -o -name 'logfile' \) -type f -exec cp {} "${RESULTS_DIR}/cluster-failure-logs/" \; 2>/dev/null || true
    echo "--- gpAdminLogs/gpinitsystem_*.log ---"
    cat "${HOME}/gpdemo/datadirs"/gpAdminLogs/gpinitsystem_*.log 2>/dev/null || echo "(none found)"
    echo "--- coordinator data directory listing ---"
    find "${HOME}/gpdemo/datadirs" -path '*qddir*' 2>/dev/null || echo "(qddir not found)"
    echo "--- coordinator log/logfile contents ---"
    find "${HOME}/gpdemo/datadirs" -path '*qddir*' \( -name '*.log' -o -name 'logfile' \) -type f -exec sh -c 'echo "=== $1 ==="; cat "$1"' _ {} \; 2>/dev/null || echo "(none found)"
    echo "::endgroup::"
    exit 1
fi
# shellcheck disable=SC1091
source "${HOME}/gpdemo/gpdemo-env.sh"
psql -p "${PGPORT}" -d postgres -c 'SELECT version();' | tee "${RESULTS_DIR}/version.txt"
echo "::endgroup::"

# --- enable the dummy_seclabel provider (built into GPHOME in the image) so the
#     integration suite's SECURITY LABEL specs work; needs a restart to load ---
if [ -f "${GPHOME}/lib/postgresql/dummy_seclabel.so" ]; then
    echo "::group::Enable dummy_seclabel"
    gpconfig -c shared_preload_libraries -v 'dummy_seclabel'
    gpstop -ra
    echo "::endgroup::"
fi

# --- env file for the test phases to source ---
cat > "${HOME}/cluster-env.sh" <<EOF
source ${GPHOME}/greenplum_path.sh
source ${HOME}/gpdemo/gpdemo-env.sh
export PATH=${HOME}/go/bin:/usr/local/go/bin:\$PATH
EOF

# --- build gpbackup binaries ---
echo "::group::Build gpbackup"
cd "${GOPATH}/src/github.com/greenplum-db/gpbackup"
make depend
make build
# gpbackup shells out to $GPHOME/bin/gpbackup_helper on every segment at
# runtime (what `make install` deploys via gpsync); without it the whole e2e
# suite fails with "Could not verify gpbackup_helper version ... No such file".
# Single-host cluster -> a plain copy covers all segments.
cp "${GOPATH}/bin/gpbackup" "${GOPATH}/bin/gprestore" "${GOPATH}/bin/gpbackup_helper" "${GPHOME}/bin/"
echo "::endgroup::"

echo "Cluster up and gpbackup built. Ready for integration / end_to_end."
