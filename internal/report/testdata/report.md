<!-- ci-resources-bot -->

## Pipeline #12345 resource report — success

### Summary

| Resource | Total |
|---|---|
| Pipeline duration | 4m 12s 🔺 +1m 12s (+40%) |
| CPU time | 60.5 s |
| Total memory (sum of peaks) | 562.0 MiB |
| Peak memory (max working set) | 412.0 MiB |
| Network RX | 9.0 MiB |
| Network TX | 3.2 MiB |
| Disk read | 720.0 MiB |
| Disk write | 260.0 MiB |

### Details

| Stage : Job | Duration | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |
|---|---|---|---|---|---|---|---|---|
| build : compile | 2m 30s 🔻 −1m 30s (−38%) | 42.5 s | 412.0 MiB | 256.0 MiB / 512.0 MiB | 250m / 500m | **41%** ⚠️ | 8.0 MiB / 3.0 MiB | 600.0 MiB / 220.0 MiB |
| test : unit | 2m 30s | 18.0 s | 150.0 MiB | 128.0 MiB / 256.0 MiB | 100m / 1000m | 2% | 1.0 MiB / 256.0 KiB | 120.0 MiB / 40.0 MiB |
| deploy : staging | — | _no data_ | | | | | | |
