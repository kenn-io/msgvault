package generated

//go:generate sh -c "go run ../../../cmd/msgvault openapi --version 3.0 --format yaml > ../openapi.yaml"
//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5 -config config.yaml ../openapi.yaml
