<!-- ci-resources-bot -->

## Pipeline #12345 resource report — success

### Summary

| Resource | Total |
|---|---|
| CPU time | 60.5 s |
| Total memory (sum of peaks) | 562.0 MiB |
| Peak memory (max working set) | 412.0 MiB |
| Network RX | 9.0 MiB |
| Network TX | 3.2 MiB |

### Details

| Stage : Job | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX |
|---|---|---|---|---|---|---|
| build : compile | 42.5 s | 412.0 MiB | 256.0 MiB / 512.0 MiB | 250m / 500m | **41%** ⚠️ | 8.0 MiB / 3.0 MiB |
| test : unit | 18.0 s | 150.0 MiB | 128.0 MiB / 256.0 MiB | 100m / 1000m | 2% | 1.0 MiB / 256.0 KiB |
| deploy : staging | _no data_ | | | | | |
