# Spring Boot LMBP demo

Minimal SaaS using [`lmbp-spring-boot-starter`](../../sdk/java). See that README for the API.

```bash
cd ../../sdk/java && mvn -q -DskipTests install
TENANT_API_KEY=$(openssl rand -hex 24) TENANT_PROFILE_FILE=../../mock/tenant-profiles.example.json \
  java -jar target/spring-boot-demo-0.1.0-SNAPSHOT.jar
# optional: LMBP_STORE=jdbc:sqlite:/tmp/lmbp.db
```

Conformance against this process: `scripts/mock-interop.sh spring-boot` (and `spring-boot-sqlite`).
