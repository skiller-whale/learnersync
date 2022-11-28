File synchronisation client for Skiller Whale learners
======================================================

This program synchronises files up to Skiller Whale as part of a live coaching
session.

Full usage instructions can be found in [src/main/resources/usage.txt](src/main/resources/usage.txt).

It is usually set up by curriculum maintainers with docker or integrated into
the [https://github.com/skiller-whale/learnerhost](hosted learner environment).

## Building

You can build the program with `go build` or use the `buildAll` script to build for all platforms.

`go test` should be pretty fast to make sure you've not broken anything.
