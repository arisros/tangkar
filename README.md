# 🍲 tangkar

Office soto tangkar orders in one shared link. Everyone adds their own bowls, and at the lock time the order goes to the seller's WhatsApp group automatically 🛵

```mermaid
flowchart TB
  P[📱 Browser] --> S[tangkar]
  T[💻 TUI] --> S
  S --> W[wa-gateway]
  W --> G[🍢 Seller group]
```

```mermaid
stateDiagram-v2
  [*] --> open: admin opens, lock 11:30
  open --> late: lock time, order sent
  late --> late: late orders, TAMBAHAN #n sent
  late --> closed: late join ends
  open --> cancelled: admin cancels
```

## 💻 TUI

```sh
go install github.com/arisros/tangkar/cmd/tangkar@latest
tangkar
```

| Key | Action |
|---|---|
| `d` / `a` | DAGING / CAMPUR |
| `+` / `-` | sate |
| `n` | note |
| `enter` | order |
| `x` | remove a bowl |
| `c` | copy link |
