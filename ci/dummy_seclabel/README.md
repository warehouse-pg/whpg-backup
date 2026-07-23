# Vendored dummy_seclabel test module

Vendored from WarehousePG `src/test/modules/dummy_seclabel`
(`EnterpriseDB/warehouse-pg-next` @ `WHPG-master`). gpbackup's integration
suite has ~29 SECURITY LABEL specs that require a label provider named `dummy`
to be loaded (`shared_preload_libraries='dummy_seclabel'`); without it they fail
with `security label provider "dummy" is not loaded`.

The module isn't shipped in the WHPG server RPM, so the CI image builds+installs
it into `$GPHOME` via PGXS (`make USE_PGXS=1 PG_CONFIG=…/pg_config install`) and
`ci/scripts/setup-cluster.bash` turns it on (`gpconfig -c shared_preload_libraries`
+ `gpstop -ra`). This mirrors what the Concourse `test-on-local-cluster.bash`
does (its `REQUIRES_DUMMY_SEC` block).
