# looters-jobs

The background work, in Go: processing the boundaries of the municipalities, starting matches, closing turns and resolving battles (read from a Kafka queue).

It is part of [Looters](https://github.com/Looters-it/looters), an online multiplayer conquest game played on real municipalities. This repository is not used on its own: it is cloned inside the root repository, which holds `docker-compose.yml`, the documentation and the instructions.

```sh
git clone https://github.com/Looters-it/looters.git && cd looters
git clone https://github.com/Looters-it/looters-jobs.git jobs
```

- Running, testing and commands: [README of the root repository](https://github.com/Looters-it/looters#readme)
- Contract between the services: [`docs/ARCHITECTURE.md`](https://github.com/Looters-it/looters/blob/main/docs/ARCHITECTURE.md)
- Rules of the game: [`docs/SPEC.md`](https://github.com/Looters-it/looters/blob/main/docs/SPEC.md)

Local checks: `go vet ./... && go test -p 1 ./...`.
