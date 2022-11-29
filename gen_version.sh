#!/bin/sh
echo "$(git describe --tags --abbrev=8 --dirty --always --long) at $(date) with $(go version) on $(hostname)" >version.txt
