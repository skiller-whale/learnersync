#!/bin/sh
# Generates a version of the form <git describe> at <date> with <go version> on <hostname>
echo "$(git describe --tags --abbrev=8 --dirty --always --long) at $(date) with $(go version) on $(hostname)" >version.txt
