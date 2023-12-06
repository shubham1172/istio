# Gateway Controller V2

## Overview

![image](./architecture.png)

## Brief

Gateway Controller V2 is replacement for Gateway Controller V1.

## Development

Installed Istio like this.

```bash
helm repo add istio https://istio-release.storage.googleapis.com/charts
helm repo update

kubectl create namespace istio-system
helm install istio-base istio/base --namespace istio-system
helm install istiod istio/istiod --namespace istio-system
# helm upgrade -n istio-system istiod istio/istiod --set pilot.image=ghcr.io/shubham1172/pilot:cv2WithLogs2 --set pilot.env.PILOT_ENABLE_GATEWAY_API=false --set pilot.env.PILOT_ENABLE_GATEWAY_API_DEPLOYMENT_CONTROLLER=false

kubectl create namespace istio-ingress
helm install istio-ingressgateway istio/gateway --namespace istio-ingress

# Install Gateway API
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.0.0/standard-install.yaml
```

Building Pilot
```bash
docker context use default
# If facing an issue with docker-credential-desktop,
# delete credStore from ~/.docker/config.json
sudo make DEBUG=1 push.docker.pilot HUB=ghcr.io/shubham1172 TAG=dev
```