#!/bin/sh

set -e
set -x

txtpbfmt pkgs/*/build.textproto && git diff --exit-code pkgs || (echo 'build.textproto files were not formatted using txtpbfmt!'; false)
