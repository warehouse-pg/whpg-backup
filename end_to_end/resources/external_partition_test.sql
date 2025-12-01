
SET statement_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET client_min_messages = warning;

SET default_with_oids = false;

--
--

-- 1. Create root partition table
CREATE TABLE logs_all (
    log_time TIMESTAMP,
    user_id INT,
    action TEXT
)
    PARTITION BY RANGE (log_time)
(
    -- Partitions are placeholders for data
    PARTITION p_20251001 START ('2025-10-01') EXCLUSIVE END ('2025-10-02'),
    PARTITION p_20251002 START ('2025-10-02') EXCLUSIVE END ('2025-10-03')
);

-- 2. Create external tables
CREATE EXTERNAL TABLE logs_20251001_ext (
    log_time TIMESTAMP,
    user_id INT,
    action TEXT
)
LOCATION ('gpfdist://host:8080/logs/2025-10-01.csv')
FORMAT 'CSV' (HEADER);

CREATE EXTERNAL TABLE logs_20251002_ext (
    log_time TIMESTAMP,
    user_id INT,
    action TEXT
)
LOCATION ('gpfdist://host:8080/logs/2025-10-02.csv')
FORMAT 'CSV' (HEADER);

-- 3. Alter root table to exchange partitions
ALTER TABLE logs_all
    EXCHANGE PARTITION p_20251001
    WITH TABLE logs_20251001_ext;

ALTER TABLE logs_all
    EXCHANGE PARTITION p_20251002
    WITH TABLE logs_20251002_ext;
