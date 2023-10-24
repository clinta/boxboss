BoxBoss is a library for managing the state of machines, comparable to tools like Ansible or Salt but solving some paintpoints with those tools.

Boxboss will be a daemon that listens for events to trigger specific configuration modules. If it's managing the state of  a file, it uses inotify to watch that file, and fix it as soon as it it changes, so it can never be out of compliance. If it's managing a service, it will listen for service events in dbus and keep that service in the correct state. This solves the scheduling problem.

Boxboss doesn't use config files. It is a library and scaffolding to build your own binaries. This menas that states can be mocked and tested in CI/CD, and allows for repsenting complex dependencies that can't be reasonably done in a config file.

This is an early WIP.
