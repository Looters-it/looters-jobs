# looters-jobs

I lavori in background, in Go: elaborazione dei confini dei comuni, avvio delle partite, chiusura dei turni e risoluzione degli scontri (letti da una coda Kafka).

Fa parte di [Looters](https://github.com/Looters-it/looters), un gioco di conquista multigiocatore sui comuni italiani. Questo repository non si usa da solo: va clonato dentro il repository radice, che contiene `docker-compose.yml`, documentazione e istruzioni.

```sh
git clone https://github.com/Looters-it/looters.git && cd looters
git clone https://github.com/Looters-it/looters-jobs.git jobs
```

- Avvio, test e comandi: [README del repository radice](https://github.com/Looters-it/looters#readme)
- Contratto tra i servizi: [`docs/ARCHITECTURE.md`](https://github.com/Looters-it/looters/blob/main/docs/ARCHITECTURE.md)
- Regole del gioco: [`docs/SPEC.md`](https://github.com/Looters-it/looters/blob/main/docs/SPEC.md)

Controlli in locale: `go vet ./... && go test -p 1 ./...`.
