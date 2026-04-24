# Miao Video Processing Service

视频处理服务，提供视频水印、分辨率调整、压缩和缩略图生成功能。

## 功能特性

- **水印添加**: 在视频中心添加45度旋转水印
- **分辨率调整**: 支持 720P、1080P、2K、4K
- **视频压缩**: 使用 H.264 编码，CRF 28
- **缩略图生成**: 自动截取第1秒帧
- **云存储上传**: 腾讯云 COS
- **回调通知**: 处理完成后自动回调

## API 接口

### 健康检查
```
GET /health
```

### 提交处理任务
```
POST /jobs
Content-Type: application/json
X-Miao-Signature: {签名}  (可选)

{
  "job_id": "唯一ID",
  "source_url": "源视频URL",
  "biz_type": "业务类型",
  "biz_id": 123,
  "watermark_template": "水印模板",
  "target_format": "mp4",
  "target_resolution": "1080P",
  "callback_url": "回调地址"
}
```

### 查询任务状态
```
GET /jobs/{job_id}
```

### 回调接口

处理完成后会向 `callback_url` 发送 POST 请求：

```
Content-Type: application/json
X-Miao-Signature: {签名}  (可选)

{
  "job_id": "任务ID",
  "status": "done|failed",
  "processed_url": "处理后视频URL",
  "thumbnail_url": "缩略图URL",
  "duration": 120.5,
  "width": 1920,
  "height": 1080,
  "watermark_applied": true,
  "compressed": true,
  "error_message": "失败原因(可选)"
}
```

| 字段 | 类型 | 说明 |
|------|------|------|
| `job_id` | string | 任务ID |
| `status` | string | `done` 成功, `failed` 失败 |
| `processed_url` | string | 处理后视频URL |
| `thumbnail_url` | string | 缩略图URL |
| `duration` | float | 视频时长(秒) |
| `width` | int | 视频宽度 |
| `height` | int | 视频高度 |
| `watermark_applied` | bool | 是否已加水印 |
| `compressed` | bool | 是否已压缩 |
| `error_message` | string | 错误信息(失败时) |

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| `PORT` | 否 | 9096 | HTTP 监听端口 |
| `WORK_DIR` | 否 | ./data | 工作目录 |
| `FFMPEG_BIN` | 否 | ffmpeg | ffmpeg 路径 |
| `WATERMARK_TEXT` | 否 | 创意喵 | 水印文字 |
| `WATERMARK_FONT` | 否 | - | 水印字体路径 |
| `CALLBACK_SECRET` | 否 | - | 回调签名密钥 |
| `COS_APP_ID` | 否 | - | 腾讯云 AppID |
| `COS_BUCKET` | 是 | - | COS Bucket 名称 |
| `COS_REGION` | 是 | - | COS 区域 |
| `COS_SECRET_ID` | 是 | - | 腾讯云 SecretID |
| `COS_SECRET_KEY` | 是 | - | 腾讯云 SecretKey |
| `COS_CDN_HOST` | 否 | - | CDN 域名 |

## 快速开始

```bash
# 配置环境变量
export COS_BUCKET=your-bucket
export COS_REGION=ap-guangzhou
export COS_SECRET_ID=your-secret-id
export COS_SECRET_KEY=your-secret-key

# 启动服务
./start.sh
```

## 项目结构

```
.
├── main.go              # 主程序入口
├── start.sh             # 启动脚本
├── restart.sh           # 重启脚本
└── data/                # 数据目录
    └── tmp/              # 临时文件
```

## License

MIT
