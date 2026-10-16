# Spring Boot LMBP 示例

使用 [`lmbp-spring-boot-starter`](../../sdk/java/README.md) 的最小 SaaS，API 见该 README。

```bash
cd ../../sdk/java && mvn -q -DskipTests install
TENANT_API_KEY=$(openssl rand -hex 24) TENANT_PROFILE_FILE=../../mock/tenant-profiles.example.json \
  java -jar ../../examples/spring-boot-demo/target/spring-boot-demo-0.1.0-SNAPSHOT.jar
# 可选：LMBP_STORE=jdbc:sqlite:/tmp/lmbp.db
```

一致性测试：`scripts/mock-interop.sh spring-boot spring-boot-sqlite`。
