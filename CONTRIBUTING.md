# 贡献指南

欢迎提交功能改进、协议适配、测试和界面优化。

提交前请执行：

~~~bash
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
go test -race ./...
python3 -m json.tool internal/app/web/openapi.json >/dev/null
~~~

涉及 Apple 登录、Cookie、IMAP 密码、验证码或邮件正文的测试必须使用合成数据。不要在提交内容、Issue、Pull Request 或截图中包含真实凭据。
