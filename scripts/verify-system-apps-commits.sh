#!/usr/bin/env sh
set -eu

checkout="${1:-data/system-apps}"
clock_commit="${CLOCK_FIX_COMMIT:-bbfcff4e02aae5ea50e60a961a5456e7af329da5}"
mlb_commit="${MLB_FIX_COMMIT:-4b284f1972a46937a3e82e4c986e65d00b975715}"

test -d "$checkout/.git"
git -C "$checkout" merge-base --is-ancestor "$clock_commit" HEAD
git -C "$checkout" merge-base --is-ancestor "$mlb_commit" HEAD
test -f "$checkout/apps/ogclock/og_clock.star"
test -f "$checkout/apps/mlb_game/mlb_game.star"

printf 'system apps verified commit=%s clock=%s mlb=%s\n' \
  "$(git -C "$checkout" rev-parse HEAD)" "$clock_commit" "$mlb_commit"
