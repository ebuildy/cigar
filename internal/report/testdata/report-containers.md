<!-- ci-resources-bot -->

## Pipeline #777 resource report — success

### Summary

| Resource | Total |
|---|---|
| Pipeline duration | 2m 00s |
| CPU time | 42.5 s |
| Total memory (sum of peaks) | 412.0 MiB |
| Peak memory (max working set) | 412.0 MiB |
| Network RX | 8.0 MiB |
| Network TX | 3.0 MiB |
| Disk read | 600.0 MiB |
| Disk write | 220.0 MiB |

### Details

| Stage : Job | CPU time | Peak memory | Mem req / limit | CPU req / limit | Throttled | Network RX / TX | Disk R / W |
|---|---|---|---|---|---|---|---|
| build : compile | 42.5 s | 412.0 MiB | 256.0 MiB / 512.0 MiB | 250m / 500m | **35%** ⚠️ | 8.0 MiB / 3.0 MiB | 600.0 MiB / 220.0 MiB |
| ↳ build | 39.8 s | 380.0 MiB | 128.0 MiB / 256.0 MiB | 150m / 250m | 12% | — / — | 580.0 MiB / 200.0 MiB |
| ↳ helper | 2.3 s | 24.0 MiB | 128.0 MiB / 256.0 MiB | 100m / 250m | **58%** ⚠️ | — / — | 20.0 MiB / 20.0 MiB |
| ↳ db | 0.4 s | 8.0 MiB | — / — | — / — | — | — / — | — / — |
