#!/bin/sh
# psql 包装：连 qa_stats30m（pgpass 按库名匹配，临时库不在里面，故显式给口令）
export PGPASSWORD=539b2be83a6df4214b344c5c776273ab
exec /.keys/bin/psql -h 127.0.0.1 -U lingshu_test -d qa_stats30m "$@"
