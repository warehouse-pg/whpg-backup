# Vendored gpdemo (single-host demo cluster)

These files are vendored verbatim from WarehousePG's `gpAux/gpdemo`
(`EnterpriseDB/warehouse-pg-next` @ `WHPG-master`, the WHPG19 source), used by
`ci/scripts/setup-cluster.bash` to stand up the demo cluster the integration /
end-to-end suites run against.

They are vendored (rather than checked out at CI time) so the pipeline's only
cross-repo dependency is the WHPG RPM `builds` artifact from
`warehouse-pg-packaging` — mirroring whpg-dr, whose `CROSS_REPO_TOKEN` only
needs `Actions:read` on that packaging repo and never checks out server source.

Driven via the vendored `Makefile` (`make create-demo-cluster`), NOT by calling
`demo_cluster.sh` directly — the Makefile translates `PORT_BASE` into
`DEMO_PORT_BASE` (the var the script actually reads) and sets `WITH_STANDBY` /
the postgres addons file. Running the script bare leaves `DEMO_PORT_BASE` empty,
so gpinitsystem aborts with "COORDINATOR_PORT variable not set".

`demo_cluster.sh` is otherwise self-contained apart from two siblings kept here:
`lalshell` (the gpinitsystem TRUSTED_SHELL loopback wrapper) and
`generate_certs.sh` (only used for the SSL variant). `probe_config.sh` is the
post-init sanity probe. The `Makefile`'s `-include ../../src/Makefile.global` is
a no-op here (that path doesn't exist in this vendored copy) and is ignored.

demo_cluster.sh is version-tolerant (it just generates a gpinitsystem config and
runs it), so one copy serves the WHPG19/7/6 lanes. If a major ever needs a
different gpdemo, refresh from that major's source tree.
