#!/bin/sh
# The GOPROXY a job's Go commands ask (#2533): the proxy $1 twice, then the module's source. A pipe falls back on any
# error, a download the proxy cuts short included; a comma only on 404 and 410. The second entry is the same proxy
# with a trailing slash: Go keeps an answer, an error too, per entry as written, so the same entry twice would never
# ask again. ci.yml's tidy job pins that against a proxy answering 500 once. go.sum and GOSUMDB verify whatever
# answers, the source included.
echo "$1|$1/|direct"
