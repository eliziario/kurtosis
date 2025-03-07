---
title: enclave inspect
sidebar_label: enclave inspect
slug: /enclave-inspect
---

To view detailed information about a given enclave, including its status and contents, run:

```bash
kurtosis enclave inspect $THE_ENCLAVE_IDENTIFIER 
```

where `$THE_ENCLAVE_IDENTIFIER` is the [resource identifier](../resource-identifier.md) for the enclave.

Running the above command will print detailed information about:

- The enclave's status (running or stopped)
- The services inside the enclave (if any), and the information for accessing those services' ports from your local machine

By default, UUIDs are shortened. To view the full UUIDs of your resources, add the following flag:
* `--full-uuids`

