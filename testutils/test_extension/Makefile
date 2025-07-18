EXTENSION = test_backup
MODULE = test_backup

DATA = test_backup--1.0.sql
PG_CPPFLAGS = -fPIC -I$(libpq_srcdir)
PG_LIBS = $(libpq_pgport)

PGXS := $(shell pg_config --pgxs)
include $(PGXS)