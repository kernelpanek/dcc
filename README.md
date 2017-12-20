## Dangling Container Checker

The purpose of Dangling Container Checker is to watch over Docker and Kubernetes to ensure 
containers are not being forgotten by Kubernetes.  When a dangling container is found, 
DCC can either report it to Kubernetes Events or kill the container.  

### How to Deploy
Deploy this utility as a DaemonSet to monitor your entire Kubernetes cluster.

### How to Configure
#### config.yaml Settings
Optionally, along side the DaemonSet, deploy a ConfigMap with `config.yaml` as the data  
and mount it in the DCC as a `volumeMount` in `/config`.

##### Timing
The interval between checks and the stop grace period timeout are both configurable 
with `check_interval` and `stop_timeout`.

```yaml
timing:
  check_interval: 60
  stop_timeout: 30
```

##### Whitelisting Image Names
Some containers are not kept in Kubernetes records, such as the "pause" container. 
By adding its image name to the whitelist, ContainerChk will ignore these containers 
in Docker. It's also feasible that you may not want to run ContainerChk as a Kubernetes 
workload, so "containerchk" is also added to the image whitelist by default.

```yaml
whitelist:
  images:
    - gcr.io/google_containers/pause-amd64
    - containerchk
```
