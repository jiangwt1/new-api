# new-api 本地构建部署指南

## 一、前端修改流程

### 1. 安装依赖

```bash
cd web
npm i
```

### 2. 本地开发调试

```bash
cd web
npm run dev
```

浏览器访问 `http://localhost:5173`

### 3. 构建前端

```bash
cd web
npm run build
```

构建产物在 `web/dist/` 目录。

---

## 二、部署流程

### 1. 上传项目到服务器

将整个项目上传到服务器（包含 `web/dist/` 目录）：

```bash
scp -r ./new-api root@服务器IP:/root/
```

### 2. 修改 docker-compose.yml

```bash
cd /root/new-api
vi docker-compose.yml
```

将 `image: calciumion/new-api:latest` 改成 `image: new-api:local`

### 3. 构建并启动

```bash
docker build -t new-api:local .
docker compose down && docker compose up -d
```

---

## 三、日常修改更新流程

### 只改了后端（Go 代码）

```bash
# 1. 上传修改的 Go 文件（示例）
scp controller/channel-test.go root@服务器IP:/root/new-api/controller/
scp relay/common/relay_info.go root@服务器IP:/root/new-api/relay/common/

# 2. 重新构建并重启（一条命令搞定）
cd /root/new-api
docker build -t new-api:local . && docker compose down && docker compose up -d
```

> 注意：docker-compose 的 service 名称是 `new-api`，不是容器名 `new-api`

### 只改了前端（React 代码）

```bash
# 1. 本地构建前端
cd web
npm run build

# 2. 上传 dist 目录
scp -r web/dist root@服务器IP:/root/new-api/web/

# 3. 重新构建并重启
cd /root/new-api
docker build -t new-api:local . && docker compose down && docker compose up -d
```

### 前后端都改了

```bash
# 1. 本地构建前端
cd web
npm run build

# 2. 上传整个项目（排除 node_modules）
rsync -av --exclude='web/node_modules' ./new-api root@服务器IP:/root/

# 3. 重新构建并重启
cd /root/new-api
docker build -t new-api:local . && docker compose down && docker compose up -d
```

### 批量上传多个 Go 文件

```bash
# 上传多个目录
scp -r controller/ relay/ service/ model/ root@服务器IP:/root/new-api/

# 上传单个文件
scp controller/channel-test.go root@服务器IP:/root/new-api/controller/
```

---

## 四、常用命令

```bash
# 查看日志
docker logs -f new-api

# 查看容器状态
docker compose ps

# 重启服务
docker compose down && docker compose up -d

# 停止服务
docker compose down

# 启动服务
docker compose up -d

# 进入容器调试
docker compose exec new-api sh

# 删除旧镜像（清理空间）
docker rmi new-api:local
```

---

## 五、注意事项

1. **不要上传 `web/node_modules`** — 已经配置了 `.dockerignore`，但如果用 rsync/scp 要手动排除
2. **数据库和 Redis 是外部的** — 需要确保 `docker-compose.yml` 中的环境变量配置正确
3. **每次更新都要重新构建镜像** — 前端文件是打包在 Go 二进制里的

---

## 六、目录结构说明

```
new-api/
├── Dockerfile          # 只编译 Go 后端
├── docker-compose.yml  # 容器编排（MySQL/Redis 用外部服务）
├── .dockerignore       # 排除 node_modules 等不需要的文件
├── web/
│   ├── src/            # 前端源码
│   ├── dist/           # 前端构建产物（npm run build 生成）
│   ├── package.json    # 前端依赖
│   └── .npmrc          # npm 配置（legacy-peer-deps=true）
├── controller/         # Go 请求处理
├── relay/              # AI API 转发
├── model/              # 数据模型
└── service/            # 业务逻辑
```