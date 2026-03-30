-- Create a config table and register it via pg_extension_config_dump.
-- gpbackup should include data for tables listed in pg_extension.extconfig.

CREATE TABLE test_extension_config_dump_config(
    id int,
    val text
) DISTRIBUTED BY (id);

SELECT pg_extension_config_dump('test_extension_config_dump_config'::regclass, '');
