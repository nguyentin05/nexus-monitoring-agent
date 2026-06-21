#!/usr/bin/env bash

INPUT_FILE=$1
START_LINE=$(head -n1 "$INPUT_FILE")
ALLOWED_TYPES="feat|fix|docs|style|refactor|test|chore|ci|build"
PATTERN="^($ALLOWED_TYPES)(\([a-zA-Z0-9_-]+\))?: .+"

if ! [[ "$START_LINE" =~ $PATTERN ]]; then
  echo "ERROR: Commit message does not follow the Conventional Commits format!"
  echo "Required format: <type>(<scope>): <subject>"
  echo "Valid types: $ALLOWED_TYPES"
  echo "Your commit: $START_LINE"
  exit 1
fi