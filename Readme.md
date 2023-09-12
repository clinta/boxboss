BoxBoss is a library for managing the state of machines, comparable to tools like Ansible, Puppet and Chef.

Takes inspiration from [mgmt](https://github.com/purpleidea/mgmt).

## What inspiration from mgmt
1. Compiled
2. Event driven. File management is trigged by inotify. Package states are triggered by package state changes.

## Why not just use mgmt?
I did not want another DSL. I want to write my configurations in a language that already has wide support and tooling. I know the golden rule is to not mix logic and configuration, but it always happens anyway. Ansible configs are full of jinja, and an extended version of jinja that adds even more logic. Mgmt's DSL explicitly supports logic, and is working on lambdas for even more logic. I want to just use go. Using go directly means we have many more mature tools for testing, CI/CD ect... The downside is compiling unique binaries for different roles. But having this automated in CI/CD, and given the speed of the go compiler, this is a worth-while tradeoff.
