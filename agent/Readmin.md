# 构建
```
go mod tidy
go build -o k8s-cd ./cmd/k8s-cd
```

# 运行（集群内ServiceAccount）
```
kubectl create sa cicd-sa
kubectl create clusterrolebinding cicd-binding --clusterrole=edit --serviceaccount=default:cicd-sa
./k8s-cd -config config.yaml
```

# 运行（kubeconfig）
```
./k8s-cd -config config.yaml
```