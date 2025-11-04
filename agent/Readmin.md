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
```
唯一标识：使用 UUID v4 (标准库 github.com/google/uuid) 作为 TaskID – 全局唯一、无冲突，支持并发生成。保留原有 service-version-env 作为复合键 (防业务重)，但 TaskID 为主键。
排序机制：

存储时：设置 CreatedAt = time.Now() (精确到纳秒)，作为排序字段。
Mongo 索引：为 created_at 添加升序索引 ({created_at: 1})，确保插入顺序 = 查询顺序。
查询时：所有查询方法添加 .Sort(bson.D{{"created_at", 1}}) – 按创建时间升序返回/执行。


并发安全：UUID 生成原子；Mongo Upsert + 唯一复合索引 (service+version+environment+created_at) 防重；Go 协程安全 (当前已用 Mutex 在 sentTasks)。
兼容性：最小修改 – 只改模型/存储/查询；后续执行系统查询时，按 created_at 排序执行。
性能：UUID 生成 O(1)；索引查询 O(log n)；适合高并发 (Mongo 原子操作)。
扩展：若需自定义顺序，可加 SequenceID (原子递增)，但 UUID+时间戳 够用/简单。

## ============

测试示例 (用 code_execution 验证 UUID/排序)：

生成 3 UUID：无冲突。
模拟插入/查询：按 CreatedAt 排序返回。


后续执行系统：查询时加 sort: created_at asc – 按顺序执行。
```
```

#!/bin/bash

# MongoDB连接配置
MONGO_HOST="localhost"
MONGO_PORT="27017"
MONGO_DB=""  # 留空则查询所有数据库
MONGO_USER=""
MONGO_PASS=""
AUTH_DB="admin"  # 认证数据库

# 输出配置
OUTPUT_FORMAT="json"  # 可选: json, csv
OUTPUT_DIR="./mongo_data_$(date +%Y%m%d_%H%M%S)"

# 显示用法
usage() {
    echo "用法: $0 [选项]"
    echo "选项:"
    echo "  -h <host>        MongoDB主机 (默认: localhost)"
    echo "  -P <port>         MongoDB端口 (默认: 27017)"
    echo "  -d <database>     指定数据库 (默认: 所有数据库)"
    echo "  -u <username>     用户名"
    echo "  -p <password>     密码"
    echo "  -f <format>       输出格式 json/csv (默认: json)"
    echo "  -o <output_dir>   输出目录 (默认: 按时间戳生成)"
    echo "  --help            显示此帮助信息"
}

# 解析命令行参数
while [[ $# -gt 0 ]]; do
    case $1 in
        -h)
            MONGO_HOST="$2"
            shift 2
            ;;
        -P)
            MONGO_PORT="$2"
            shift 2
            ;;
        -d)
            MONGO_DB="$2"
            shift 2
            ;;
        -u)
            MONGO_USER="$2"
            shift 2
            ;;
        -p)
            MONGO_PASS="$2"
            shift 2
            ;;
        -f)
            OUTPUT_FORMAT="$2"
            shift 2
            ;;
        -o)
            OUTPUT_DIR="$2"
            shift 2
            ;;
        --help)
            usage
            exit 0
            ;;
        *)
            echo "未知选项: $1"
            usage
            exit 1
            ;;
    esac
done

# 构建连接字符串
AUTH_STRING=""
if [[ -n "$MONGO_USER" && -n "$MONGO_PASS" ]]; then
    AUTH_STRING="--username $MONGO_USER --password $MONGO_PASS --authenticationDatabase $AUTH_DB"
fi

CONNECTION_STRING="--host $MONGO_HOST --port $MONGO_PORT"

# 创建输出目录
mkdir -p "$OUTPUT_DIR"

echo "开始导出MongoDB数据..."
echo "连接: $MONGO_HOST:$MONGO_PORT"
echo "输出目录: $OUTPUT_DIR"
echo "格式: $OUTPUT_FORMAT"
echo "----------------------------------------"

# 获取数据库列表函数
get_databases() {
    if [[ -n "$MONGO_DB" ]]; then
        echo "$MONGO_DB"
    else
        mongo $CONNECTION_STRING $AUTH_STRING --quiet --eval "db.getMongo().getDBNames()" | tr -d '[]",' | tr ' ' '\n' | grep -v '^$'
    fi
}

# 获取集合列表函数
get_collections() {
    local db="$1"
    mongo $CONNECTION_STRING $AUTH_STRING --quiet --eval "db.getSiblingDB('$db').getCollectionNames()" | tr -d '[]",' | tr ' ' '\n' | grep -v '^$'
}

# 导出集合数据函数
export_collection() {
    local db="$1"
    local collection="$2"
    local output_file="$OUTPUT_DIR/${db}_${collection}.$OUTPUT_FORMAT"
    
    echo "正在导出: $db.$collection"
    
    if [[ "$OUTPUT_FORMAT" == "csv" ]]; then
        mongoexport $CONNECTION_STRING $AUTH_STRING --db "$db" --collection "$collection" --type=csv --out "$output_file" 2>/dev/null
    else
        mongoexport $CONNECTION_STRING $AUTH_STRING --db "$db" --collection "$collection" --jsonArray --out "$output_file" 2>/dev/null
    fi
    
    if [[ $? -eq 0 && -f "$output_file" ]]; then
        local file_size=$(wc -l < "$output_file" 2>/dev/null || echo "0")
        echo "✓ 完成: $output_file ($file_size 行)"
    else
        echo "✗ 失败: $db.$collection"
    fi
}

# 主执行逻辑
total_collections=0
success_count=0

for database in $(get_databases); do
    echo "数据库: $database"
    echo "----------------------------------------"
    
    for collection in $(get_collections "$database"); do
        # 跳过系统集合
        if [[ "$collection" == system.* ]]; then
            continue
        fi
        
        export_collection "$database" "$collection"
        ((total_collections++))
        if [[ $? -eq 0 ]]; then
            ((success_count++))
        fi
        echo ""
    done
done

echo "========================================"
echo "导出完成!"
echo "总计: $total_collections 个集合"
echo "成功: $success_count 个"
echo "失败: $((total_collections - success_count)) 个"
echo "数据保存至: $OUTPUT_DIR"

# 生成汇总报告
echo "{
  \"export_summary\": {
    \"timestamp\": \"$(date -Iseconds)\",
    \"host\": \"$MONGO_HOST:$MONGO_PORT\",
    \"total_collections\": $total_collections,
  \"success_count\": $success_count,
  \"failed_count\": $((total_collections - success_count)),
  \"output_directory\": \"$OUTPUT_DIR\",
    \"format\": \"$OUTPUT_FORMAT\"
  }
}" > "$OUTPUT_DIR/export_summary.json"

```