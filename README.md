# File synchronisation client for Skiller Whale learners

This program synchronises files up to Skiller Whale as part of a live coaching
session.

SkillerWhaleSync scans for changes on a local filesystem and posts them up to
Skiller Whale for a coach to view during a session.

It is usually set up by curriculum maintainers with docker or integrated into
the [https://github.com/skiller-whale/learnerhost](hosted learner environment),
so this explanation isn't written for learners!

## Simple integration

For curriculum authors, you add synchronisation to your curriculum by
including this service in its docker-compose.yml:

```yaml
  sync:
    image: "ghcr.io/skiller-whale/learnersync"
    network_mode: "host"
    environment:
      WATCHER_BASE_PATH: "/app/exercises"
      ATTENDANCE_ID_FILE: "/app/sync/attendance_id"
      WATCHED_EXTS: "js jsx ts tsx html"
      IGNORE_DIRS: ".git"
    volumes:
      - "./src:/app/exercises/src"
      - "./attendance_id:/app/sync/attendance_id"
    tty: true
    stdin_open: true
```

This should always reference the latest-tested Docker image.

If you need to test pre-release versions, you can add change the image
argument to e.g. `gchr.io/skiller-whale/learnersync:pre-release-branch`.

## Building

You can build the program with `go build` or use the `buildAll` script to build for all platforms.

`go test` should be pretty fast to make sure you've not broken anything.

## Supplying the attendance id

Each learner in a coaching session is given an "attendance id", a random number which they need to pass on
to SkillerWhaleSync.  The program provides three ways of doing this:

1. manually writing it to a file in the exercise folder usually called `attendance_id`
2. starting the program with the ATTENDANCE_ID parameter (the hosted learner environment does this)
3. issuing an HTTP GET to `http://localhost:9494/set?id=...&redirect=...` - an experimental half-way house
   for train to use.

## Polling vs inotify

SkillerWhaleSync uses the Go fsnotify library which asks the OS to call it back when there are file changes.
This doesn't always work (e.g. on Docker and Windows), so initially the program will also start polling the
filesystem every 2.5s and report changes that way.

Once a notification has been received, it will disable the less-efficient polling.

But you can force it to poll using the *FORCE_POLL* parameter below.

## Setup

The program is configured through environment variables, only the first of which is compulsory:

* **WATCHED_EXTS**: Space-separated list of file extensions to monitor, all others are ignored e.g. `js ts`.
* **IGNORE_MATCH**: A space-separated list of patterns to ignore in the full pathname e.g. `.git .DS_Store *.class`.
* **SERVER_URL**: The URL of the server which has `/pings` and `/file_snapshots` endpoints, defaults to `https://train.skillerwhale.com`.
* **ATTENDANCE_ID_FILE**: The file in which the user will write their session "attendance_id" supplied by the training interface to identify a particular learner. The program will not start until a valid ID is written to this file.
* **ATTENDANCE_ID**: Alternatively, you can supply the ID directly and the program will quit if it's not valid.
* **WATCHED_BASE_PATH**: The directory to monitor for file changes (defaults to `.`).
* **TRIGGER_EXEC**: A trigger program to run on every file change with the full pathname as its argument.
* **FORCE_POLL**: Set to "1" to disable inotify usage on the host.
