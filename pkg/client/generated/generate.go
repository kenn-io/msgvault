package generated

//go:generate sh -c "go run ../../../cmd/msgvault openapi --version 3.0 --format yaml > ../openapi.yaml"
//go:generate go tool -modfile=../../../tools/oapi-codegen/go.mod oapi-codegen -config config.yaml ../openapi.yaml
