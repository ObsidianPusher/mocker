# Mocker

Mocker 是一个轻量级、面向边缘设备的服务守护与 Web 控制台，用于管理受限根目录下的可执行文件、查看日志、监控资源并提供单页管理界面。

## 特性

- 受限根目录访问
- 服务自启动与手动控制
- 日志查看与文件编辑
- 服务资源监控
- HTTPS 管理控制台

## 构建

```sh
TARGET_ARCH=arm64 ./build-musl.sh
```

构建产物位于 `dist/mocker-linux-amd64-musl`。

## 运行

1. 创建 `config.json`，可参考 `config.example.json`。
2. 启动程序：`./dist/mocker-linux-amd64-musl --config config.json`
3. 确保证书和私钥文件可被 `mocker` 读取。
4. 打开 `https://localhost:8090`。
5. 默认账号为 `admin`，默认密码为 `admin`，首次启动后可在“账号”页修改。

## 配置示例

```json
{
  "roots": ["data"],
  "services": [
    {
      "id": "goclaw",
      "name": "GoClaw",
      "path": "/mnt/dm-1/.ugreen_nas/830402/data/openclaw/bin/goclaw",
      "args": [],
      "auto_start": true,
      "restart_on_exit": true
    }
  ],
  "http": {
    "address": "0.0.0.0:20228",
    "cert_file": "/etc/ssl/certs/server.crt",
    "key_file": "/etc/ssl/private/server.key"
  }
}
```

## 行为说明

- `http.address` 是控制台 HTTPS 监听地址。
- 服务配置不再包含 `port`、`https_port`、`env_enabled` 或 `env`。
- 服务运行状态只展示进程与资源信息，不再做端口映射管理。
- 如需修改控制台 HTTPS 监听地址、证书或私钥，保存后重启进程生效。

## 测试

```sh
go test ./...
```
