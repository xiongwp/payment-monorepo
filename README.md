# Accounting Admin Web

Accounting System的管理后台，提供账户管理、交易查询、调账审批等功能。

## 功能特性

- ✅ **数据概览**：实时统计、趋势图表
- ✅ **账户管理**：账户查询、创建、冻结、解冻
- ✅ **交易流水**：交易查询、导出
- ✅ **余额快照**：每日余额快照查看
- ✅ **调账管理**：调账申请和审批
- ✅ **日切管理**：触发日切、查看日切状态
- ✅ **响应式设计**：支持桌面和移动端

## 技术栈

- **React 18** - UI框架
- **TypeScript** - 类型安全
- **Ant Design 5** - UI组件库
- **Ant Design Pro Components** - 高级组件
- **React Router 6** - 路由管理
- **Recharts** - 图表库
- **Zustand** - 状态管理
- **Axios** - HTTP客户端
- **Vite** - 构建工具

## 快速开始

### 1. 安装依赖

```bash
npm install
# 或
yarn install
# 或
pnpm install
```

### 2. 启动开发服务器

```bash
npm run dev
```

访问 http://localhost:3000

### 3. 构建生产版本

```bash
npm run build
```

构建产物在 `dist/` 目录。

### 4. 预览生产版本

```bash
npm run preview
```

## 项目结构

```
accounting-admin-web/
├── src/
│   ├── pages/              # 页面组件
│   │   ├── Dashboard/      # 工作台
│   │   ├── Account/        # 账户管理
│   │   ├── Transaction/    # 交易流水
│   │   ├── Snapshot/       # 余额快照
│   │   ├── Adjustment/     # 调账管理
│   │   └── DayCut/         # 日切管理
│   ├── layouts/            # 布局组件
│   ├── components/         # 通用组件
│   ├── services/           # API服务
│   ├── utils/              # 工具函数
│   ├── App.tsx             # 应用入口
│   └── main.tsx            # 主入口
├── public/                 # 静态资源
├── index.html              # HTML模板
├── vite.config.ts          # Vite配置
├── tsconfig.json           # TypeScript配置
└── package.json            # 项目配置
```

## 功能截图

### 工作台
- 实时数据统计
- 交易趋势图表
- 最近交易列表

### 账户管理
- 账户列表
- 账户详情
- 账户冻结/解冻
- 创建账户

### 交易流水
- 交易查询
- 高级筛选
- 交易详情

### 调账管理
- 调账申请
- 审批流程
- 调账记录

### 日切管理
- 触发日切
- 日切进度
- 日切记录

## API集成

系统通过Vite的proxy功能连接到gRPC API：

```typescript
// vite.config.ts
export default defineConfig({
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:9090',
        changeOrigin: true,
      },
    },
  },
})
```

## 环境变量

创建 `.env.local` 文件：

```bash
# API地址
VITE_API_BASE_URL=http://localhost:9090

# 应用标题
VITE_APP_TITLE=Accounting System
```

## 开发指南

### 添加新页面

1. 在 `src/pages/` 创建页面组件
2. 在 `src/App.tsx` 添加路由
3. 在 `src/layouts/AppLayout.tsx` 添加菜单项

### 添加新API

1. 在 `src/services/` 创建API服务
2. 使用axios发起HTTP请求
3. 在组件中调用API

### 代码规范

```bash
# 代码检查
npm run lint

# 代码格式化
npm run format
```

## 依赖关系

```
accounting-admin-web (本仓库)
    ↓ 调用
accounting-grpc-api (gRPC接口)
    ↓ 调用
accounting-system (核心服务)
```

## 性能优化

- 使用React.lazy进行代码分割
- 图表使用虚拟滚动
- 列表使用分页加载
- 图片懒加载
- 路由预加载

## 浏览器支持

- Chrome (最新版)
- Firefox (最新版)
- Safari (最新版)
- Edge (最新版)

## 部署

### Docker部署

```bash
# 构建镜像
docker build -t accounting-admin-web .

# 运行容器
docker run -p 80:80 accounting-admin-web
```

### Nginx配置

```nginx
server {
    listen 80;
    server_name your-domain.com;

    location / {
        root /usr/share/nginx/html;
        index index.html;
        try_files $uri $uri/ /index.html;
    }

    location /api {
        proxy_pass http://grpc-api:9090;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

## 相关链接

- 核心服务: [accounting-system](https://github.com/xiongwp/accounting-system)
- gRPC API: [accounting-grpc-api](https://github.com/xiongwp/accounting-grpc-api)
- Ant Design: https://ant.design/
- React: https://react.dev/

## License

MIT
