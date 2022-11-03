File synchronisation client for Skiller Whale learners
======================================================

This program synchronises files up to Skiller Whale as part of a live coaching
session.

Full usage instructions can be found in [src/main/resources/usage.txt](src/main/resources/usage.txt).

It is usually set up by curriculum maintainers with docker or integrated into
the [https://github.com/skiller-whale/learnerhost](hosted learner environment).

## Building

The project is build through Gradle

### Java version (using ordinary JDK)

`./gradlew build`
will output to `build/libs/SkillerWhale.jar`

### Native version (using GraalVM installation)

`./gradlew nativeCompile`
will output to `build/native/nativeCompile/SkillerWhaleSync`

