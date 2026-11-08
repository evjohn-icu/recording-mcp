#!/bin/sh
# Initializes the profile named by RECORDING_PROFILE inside the container data
# volume. The password is read only from MEMORY_PROFILE_PASSWORD; the script
# never creates a default password and never prints it.
set -eu

if [ -z "${MEMORY_PROFILE_PASSWORD:-}" ]; then
	echo "docker-profile-init: MEMORY_PROFILE_PASSWORD is required" >&2
	exit 1
fi

PROFILE="${RECORDING_PROFILE:-home}"
ROOT="${RECORDING_PROFILES_ROOT:-/data/profiles}"

if /app/context-profile list --profiles-root "$ROOT" | grep -qx "$PROFILE"; then
	/app/context-profile verify --profiles-root "$ROOT" --name "$PROFILE"
	echo "docker-profile-init: profile $PROFILE verified"
else
	/app/context-profile create --profiles-root "$ROOT" --name "$PROFILE"
	echo "docker-profile-init: profile $PROFILE created"
fi
