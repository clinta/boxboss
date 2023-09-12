## Core
Contains interfaces for plugins

### Plugin interface
* Check - returns bool to determine if change needs to be made
* Trigger - what triggers a Check
* Run - runs the plugin - returns an interface that indicates whether or not something changed and errors
* Conditions - only run this state if given conditions are true

#### Inherited properties of all plugins
* Dependencies - states that must be successful before this state runs
* Callbacks
  * Before Check
  * Before Change
  * After Check
  * After Change

## Plugins to create
* File
  * Contents
  * Regex Replace
  * Template
  * Mode
  * Create parent directories
    * Mode
  * Download

* User
  * Name
  * Password
  * SSH key

* Package
* Service

## How to structure host configs
* Roles
  * Can import other roles, and override settings in those roles
  * Can contain tests
  * Each roll has a main
  * Rolls can be an individual host

## How do you initialize a deployment?
* The program can change it's own roll, by downloading the different role
* Maybe the build process should create symlinks to the right roll? Or include a webserver that routes hostnames to their correct role.

## Security
* Maybe a client shouldn't be able to choose it's own roll.
* Key management? Generate a key on start, wait for approval on the server and being assigned to a roll.
* Some rolls can be deployed without approved keys for initial configuration

## First class versioning using incrementing numbers, not semver.
* Two versions - core version and config version
* Only download new roll if the version changes
* Optional websocket for instant updates - or poll
* Multiple server support, only use latest version, only connect to servers who's version matches
* How to handle rollbacks?
  * Never downgrade, rollbacks must still increment the version numbers
  * Build can dynamically insert the version number from a tag
  * A CI workflow can increment version numbers - tag then build
  * non-tagged can run last tag + timestamp?


## Misc notes
Role contains a number of states
A state is an instance of a state plugin

### State plugin interface
* Builder()
  * (plugin specific stuff, file name, mode ect...)
  * Name - used for logging ect...
  * Add Trigger - an event that triggers the state
  * Add Requirement - something that must pass or the state will not run, gets inserted into precheck
  * Add PreCheck() - function that is run before checking the state
  * Add PostCheck() - function that is run after checking the state, regardless of whether or not changes are required
  * Add PreRun() - function that is run before running the state, only run if the check indicated changes are needed
  * Add PostRun() - function that is run after the check is run
  * Add PostSuccess() - added to PostRun with condition on state
  * Add PostFailure() - added to PostRun with condition on the state
  * Build() - builds the state, spawns a goroutine waiting for triggers
* Check() - checks if a change is needed
* Run() - makes the change
* Status() - Atomic access to a status
* Events() - subscribes to a channel waiting for events from this state
* SuccessEvents() - filtered subscription
* FailureEvents() - filtered subscription
