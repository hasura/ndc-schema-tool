package openapi

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	rest "github.com/hasura/ndc-rest-schema/schema"
	"github.com/hasura/ndc-rest-schema/utils"
	"github.com/hasura/ndc-sdk-go/schema"
	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v2 "github.com/pb33f/libopenapi/datamodel/high/v2"
	"github.com/pb33f/libopenapi/orderedmap"
)

type openAPIv2Converter struct {
	schema *rest.NDCRestSchema
	*ConvertOptions
}

// OpenAPIv2ToNDCSchema converts OpenAPI v2 JSON bytes to NDC REST schema
func OpenAPIv2ToNDCSchema(input []byte, options *ConvertOptions) (*rest.NDCRestSchema, []error) {
	opts, err := validateConvertOptions(options)
	if err != nil {
		return nil, []error{err}
	}
	document, err := libopenapi.NewDocument(input)
	if err != nil {
		return nil, []error{err}
	}

	docModel, errs := document.BuildV2Model()
	// The errors won’t prevent the model from building
	if docModel == nil && len(errs) > 0 {
		return nil, errs
	}

	if docModel.Model.Paths == nil || docModel.Model.Paths.PathItems == nil || docModel.Model.Paths.PathItems.IsZero() {
		return nil, append(errs, errors.New("there is no API to be converted"))
	}

	converter := &openAPIv2Converter{
		schema:         rest.NewNDCRestSchema(),
		ConvertOptions: opts,
	}
	if docModel.Model.Info != nil {
		converter.schema.Settings.Version = docModel.Model.Info.Version
	}

	if docModel.Model.Host != "" {
		scheme := "https"
		for _, s := range docModel.Model.Schemes {
			if strings.HasPrefix(s, "http") {
				scheme = s
				break
			}
		}
		envName := utils.StringSliceToConstantCase([]string{opts.EnvPrefix, "SERVER_URL"})
		serverURL := fmt.Sprintf("%s://%s%s", scheme, docModel.Model.Host, docModel.Model.BasePath)
		converter.schema.Settings.Servers = append(converter.schema.Settings.Servers, rest.ServerConfig{
			URL: rest.NewEnvTemplateWithDefault(envName, serverURL).String(),
		})
	}

	for iterPath := docModel.Model.Paths.PathItems.First(); iterPath != nil; iterPath = iterPath.Next() {
		if err := converter.pathToNDCOperations(iterPath); err != nil {
			return nil, append(errs, err)
		}
	}

	if docModel.Model.Definitions != nil {
		for cSchema := docModel.Model.Definitions.Definitions.First(); cSchema != nil; cSchema = cSchema.Next() {
			if err := converter.convertComponentSchemas(cSchema); err != nil {
				return nil, append(errs, err)
			}
		}
	}

	if docModel.Model.SecurityDefinitions != nil && docModel.Model.SecurityDefinitions.Definitions != nil {
		converter.schema.Settings.SecuritySchemes = make(map[string]rest.SecurityScheme)
		for scheme := docModel.Model.SecurityDefinitions.Definitions.First(); scheme != nil; scheme = scheme.Next() {
			err := converter.convertSecuritySchemes(scheme)
			if err != nil {
				return nil, append(errs, err)
			}
		}
	}

	converter.schema.Settings.Security = convertSecurities(docModel.Model.Security)

	return converter.schema, nil
}

func (oc *openAPIv2Converter) convertSecuritySchemes(scheme orderedmap.Pair[string, *v2.SecurityScheme]) error {
	key := scheme.Key()
	security := scheme.Value()
	if security == nil {
		return nil
	}
	result := rest.SecurityScheme{}
	switch security.Type {
	case "apiKey":
		result.Type = rest.APIKeyScheme
		inLocation, err := rest.ParseAPIKeyLocation(security.In)
		if err != nil {
			return err
		}
		apiConfig := rest.APIKeyAuthConfig{
			In:   inLocation,
			Name: security.Name,
		}
		result.Value = rest.EnvTemplate{
			Name: utils.StringSliceToConstantCase([]string{oc.EnvPrefix, key}),
		}.String()
		result.APIKeyAuthConfig = &apiConfig
	case "basic":
		httpConfig := rest.HTTPAuthConfig{
			Scheme: "Basic",
			Header: "Authorization",
		}
		result.Value = rest.EnvTemplate{
			Name: utils.StringSliceToConstantCase([]string{oc.EnvPrefix, key, "TOKEN"}),
		}.String()
		result.HTTPAuthConfig = &httpConfig
	case "oauth2":
		var flowType rest.OAuthFlowType
		switch security.Flow {
		case "accessCode":
			flowType = rest.AuthorizationCodeFlow
		case "implicit":
			flowType = rest.ImplicitFlow
		case "password":
			flowType = rest.PasswordFlow
		case "application":
			flowType = rest.ClientCredentialsFlow
		}
		flow := rest.OAuthFlow{
			AuthorizationURL: security.AuthorizationUrl,
			TokenURL:         security.TokenUrl,
		}

		if security.Scopes != nil {
			scopes := make(map[string]string)
			for scope := security.Scopes.Values.First(); scope != nil; scope = scope.Next() {
				scopes[scope.Key()] = scope.Value()
			}
			flow.Scopes = scopes
		}
		result.Type = rest.OAuth2Scheme
		result.OAuth2Config = &rest.OAuth2Config{
			Flows: map[rest.OAuthFlowType]rest.OAuthFlow{
				flowType: flow,
			},
		}
	default:
		return fmt.Errorf("invalid security scheme: %s", security.Type)
	}

	oc.schema.Settings.SecuritySchemes[key] = result
	return nil
}

func (oc *openAPIv2Converter) pathToNDCOperations(pathItem orderedmap.Pair[string, *v2.PathItem]) error {
	pathKey := pathItem.Key()
	pathValue := pathItem.Value()
	if pathValue.Get != nil {
		itemGet := pathValue.Get
		funcName := itemGet.OperationId
		if funcName == "" {
			funcName = buildPathMethodName(pathKey, "get", oc.ConvertOptions)
		}
		resultType, err := oc.convertResponse(itemGet.Responses, pathKey, []string{funcName, "Result"})
		if err != nil {
			return fmt.Errorf("%s: %s", pathKey, err)
		}
		if resultType != nil {
			arguments, reqParams, reqBody, err := oc.convertParameters(itemGet.Parameters, pathKey, []string{funcName})
			if err != nil {
				return fmt.Errorf("%s: %s", funcName, err)
			}

			function := rest.RESTFunctionInfo{
				Request: &rest.Request{
					URL:         pathKey,
					Method:      "get",
					Parameters:  reqParams,
					RequestBody: reqBody,
					Security:    convertSecurities(itemGet.Security),
				},
				FunctionInfo: schema.FunctionInfo{
					Name:       funcName,
					Arguments:  arguments,
					ResultType: resultType.Encode(),
				},
			}

			if itemGet.Summary != "" {
				function.Description = &itemGet.Summary
			}

			oc.schema.Functions = append(oc.schema.Functions, &function)
		}
	}

	procPost, err := oc.convertProcedureOperation(pathKey, "post", pathValue.Post)
	if err != nil {
		return err
	}
	if procPost != nil {
		oc.schema.Procedures = append(oc.schema.Procedures, procPost)
	}

	procPut, err := oc.convertProcedureOperation(pathKey, "put", pathValue.Put)
	if err != nil {
		return err
	}
	if procPut != nil {
		oc.schema.Procedures = append(oc.schema.Procedures, procPut)
	}

	procPatch, err := oc.convertProcedureOperation(pathKey, "patch", pathValue.Patch)
	if err != nil {
		return err
	}
	if procPatch != nil {
		oc.schema.Procedures = append(oc.schema.Procedures, procPatch)
	}

	procDelete, err := oc.convertProcedureOperation(pathKey, "delete", pathValue.Delete)
	if err != nil {
		return err
	}
	if procDelete != nil {
		oc.schema.Procedures = append(oc.schema.Procedures, procDelete)
	}
	return nil
}

func (oc *openAPIv2Converter) convertProcedureOperation(pathKey string, method string, operation *v2.Operation) (*rest.RESTProcedureInfo, error) {

	if operation == nil {
		return nil, nil
	}

	procName := operation.OperationId
	if procName == "" {
		procName = buildPathMethodName(pathKey, method, oc.ConvertOptions)
	}

	resultType, err := oc.convertResponse(operation.Responses, pathKey, []string{procName, "Result"})
	if err != nil {
		return nil, fmt.Errorf("%s: %s", pathKey, err)
	}

	if resultType == nil {
		return nil, nil
	}

	arguments, reqParams, reqBody, err := oc.convertParameters(operation.Parameters, pathKey, []string{procName})
	if err != nil {
		return nil, fmt.Errorf("%s: %s", pathKey, err)
	}

	if reqBody != nil && len(operation.Consumes) > 0 {
		contentType := rest.ContentTypeJSON
		if !slices.Contains(operation.Consumes, rest.ContentTypeJSON) {
			contentType = operation.Consumes[0]
		}
		reqBody.ContentType = contentType
	}

	procedure := rest.RESTProcedureInfo{
		Request: &rest.Request{
			URL:         pathKey,
			Method:      method,
			Parameters:  reqParams,
			RequestBody: reqBody,
			Security:    convertSecurities(operation.Security),
		},
		ProcedureInfo: schema.ProcedureInfo{
			Name:       procName,
			Arguments:  arguments,
			ResultType: resultType.Encode(),
		},
	}

	if operation.Summary != "" {
		procedure.Description = &operation.Summary
	}

	return &procedure, nil
}

func (oc *openAPIv2Converter) convertParameters(params []*v2.Parameter, apiPath string, fieldPaths []string) (map[string]schema.ArgumentInfo, []rest.RequestParameter, *rest.RequestBody, error) {

	if len(params) == 0 {
		return map[string]schema.ArgumentInfo{}, nil, nil, nil
	}

	var reqParams []rest.RequestParameter
	arguments := make(map[string]schema.ArgumentInfo)

	var requestBody *rest.RequestBody
	formData := rest.TypeSchema{
		Type:       "object",
		Properties: make(map[string]rest.TypeSchema),
	}
	for _, param := range params {
		if param == nil {
			continue
		}
		paramName := param.Name
		if paramName == "" {
			return nil, nil, nil, errors.New("parameter name is empty")
		}

		var schemaType schema.TypeEncoder
		var typeSchema *rest.TypeSchema
		var err error

		paramRequired := false
		if param.Required != nil && *param.Required {
			paramRequired = true
		}

		if param.Type != "" {
			schemaType, err = oc.getSchemaTypeFromParameter(param, apiPath, fieldPaths)
			if err != nil {
				return nil, nil, nil, err
			}
			nullable := !paramRequired
			typeSchema = &rest.TypeSchema{
				Type:     getNamedType(schemaType, param.Type),
				Pattern:  param.Pattern,
				Nullable: nullable,
			}
			if param.Maximum != nil {
				maximum := float64(*param.Maximum)
				typeSchema.Maximum = &maximum
			}
			if param.Minimum != nil {
				minimum := float64(*param.Minimum)
				typeSchema.Minimum = &minimum
			}
			if param.MaxLength != nil {
				maxLength := int64(*param.MaxLength)
				typeSchema.MaxLength = &maxLength
			}
			if param.MinLength != nil {
				minLength := int64(*param.MinLength)
				typeSchema.MinLength = &minLength
			}
		} else if param.Schema != nil {
			schemaType, typeSchema, err = oc.getSchemaTypeFromProxy(param.Schema, !paramRequired, apiPath, fieldPaths)
			if err != nil {
				return nil, nil, nil, err
			}
		}

		paramLocation, err := rest.ParseParameterLocation(param.In)
		if err != nil {
			return nil, nil, nil, err
		}

		argument := schema.ArgumentInfo{
			Type: schemaType.Encode(),
		}
		if param.Description != "" {
			argument.Description = &param.Description
		}

		switch paramLocation {
		case rest.InBody:
			arguments["body"] = argument
			requestBody = &rest.RequestBody{
				Schema: typeSchema,
			}
		case rest.InFormData:
			arguments[paramName] = argument
			if typeSchema != nil {
				formData.Properties[paramName] = *typeSchema
			}
		default:
			arguments[paramName] = argument
			reqParams = append(reqParams, rest.RequestParameter{
				Name:   paramName,
				In:     paramLocation,
				Schema: typeSchema,
			})
		}
	}

	if len(formData.Properties) > 0 {
		requestBody = &rest.RequestBody{
			ContentType: rest.ContentTypeMultipartFormData,
			Schema:      &formData,
		}
	}

	return arguments, reqParams, requestBody, nil

}

// get and convert an OpenAPI data type to a NDC type
func (oc *openAPIv2Converter) getSchemaTypeFromProxy(schemaProxy *base.SchemaProxy, nullable bool, apiPath string, fieldPaths []string) (schema.TypeEncoder, *rest.TypeSchema, error) {

	if schemaProxy == nil {
		return nil, nil, errParameterSchemaEmpty
	}
	innerSchema := schemaProxy.Schema()
	if innerSchema == nil {
		return nil, nil, fmt.Errorf("cannot get schema from proxy: %s", schemaProxy.GetReference())
	}
	var ndcType schema.TypeEncoder
	var typeSchema *rest.TypeSchema
	var err error

	refName := getSchemaRefTypeNameV2(schemaProxy.GetReference())
	// return early object from ref
	if refName != "" && len(innerSchema.Type) > 0 && innerSchema.Type[0] == "object" {
		ndcType = schema.NewNamedType(utils.ToPascalCase(refName))
		typeSchema = &rest.TypeSchema{Type: refName}
	} else {
		if innerSchema.Title != "" && !strings.Contains(innerSchema.Title, " ") {
			fieldPaths = []string{utils.ToPascalCase(innerSchema.Title)}
		}
		ndcType, typeSchema, err = oc.getSchemaType(innerSchema, apiPath, fieldPaths)
		if err != nil {
			return nil, nil, err
		}
	}
	if nullable {
		ndcType = schema.NewNullableType(ndcType)
	}
	return ndcType, typeSchema, nil
}

// get and convert an OpenAPI data type to a NDC type from parameter
func (oc *openAPIv2Converter) getSchemaTypeFromParameter(param *v2.Parameter, apiPath string, fieldPaths []string) (schema.TypeEncoder, error) {

	if param.Type == "" {
		return nil, errParameterSchemaEmpty
	}

	var result schema.TypeEncoder
	if isPrimitiveScalar(param.Type) {
		scalarName := getScalarFromType(oc.schema, []string{param.Type}, param.Format, param.Enum, oc.trimPathPrefix(apiPath), fieldPaths)
		result = schema.NewNamedType(scalarName)
	} else {
		switch param.Type {
		case "object":
			return nil, errors.New("unsupported object parameter")
		case "array":
			if param.Items == nil && param.Items.Type == "" {
				return nil, errors.New("array item is empty")
			}

			itemName := getScalarFromType(oc.schema, []string{param.Items.Type}, param.Format, param.Enum, oc.trimPathPrefix(apiPath), fieldPaths)
			result = schema.NewArrayType(schema.NewNamedType(itemName))

		default:
			return nil, fmt.Errorf("unsupported schema type %s", param.Type)
		}
	}

	if param.Required == nil || !*param.Required {
		return schema.NewNullableType(result), nil
	}
	return result, nil
}

// get and convert an OpenAPI data type to a NDC type
func (oc *openAPIv2Converter) getSchemaType(typeSchema *base.Schema, apiPath string, fieldPaths []string) (schema.TypeEncoder, *rest.TypeSchema, error) {

	if typeSchema == nil {
		return nil, nil, errParameterSchemaEmpty
	}

	var typeResult *rest.TypeSchema
	if len(typeSchema.AnyOf) > 0 || typeSchema.AdditionalProperties != nil || len(typeSchema.Type) > 1 {
		scalarName := "JSON"
		if _, ok := oc.schema.ScalarTypes[scalarName]; !ok {
			oc.schema.ScalarTypes[scalarName] = *schema.NewScalarType()
		}
		typeResult = createSchemaFromOpenAPISchema(typeSchema, scalarName)
		return schema.NewNamedType(scalarName), typeResult, nil
	}

	if len(typeSchema.Type) == 0 {
		return nil, nil, errParameterSchemaEmpty
	}

	var result schema.TypeEncoder
	typeName := typeSchema.Type[0]
	if isPrimitiveScalar(typeName) {
		scalarName := getScalarFromType(oc.schema, typeSchema.Type, typeSchema.Format, typeSchema.Enum, oc.trimPathPrefix(apiPath), fieldPaths)
		result = schema.NewNamedType(scalarName)
		typeResult = createSchemaFromOpenAPISchema(typeSchema, scalarName)
	} else {

		typeResult = createSchemaFromOpenAPISchema(typeSchema, "")
		typeResult.Type = typeName
		switch typeName {
		case "object":
			refName := utils.StringSliceToPascalCase(fieldPaths)

			if typeSchema.Properties == nil || typeSchema.Properties.IsZero() {
				// treat no-property objects as a JSON scalar
				oc.schema.ScalarTypes[refName] = *schema.NewScalarType()
			} else {
				object := schema.ObjectType{
					Fields: make(schema.ObjectTypeFields),
				}
				if typeSchema.Description != "" {
					object.Description = &typeSchema.Description
				}

				typeResult.Properties = make(map[string]rest.TypeSchema)
				for prop := typeSchema.Properties.First(); prop != nil; prop = prop.Next() {
					propName := prop.Key()
					nullable := !slices.Contains(typeSchema.Required, propName)
					propType, propApiSchema, err := oc.getSchemaTypeFromProxy(prop.Value(), nullable, apiPath, append(fieldPaths, propName))
					if err != nil {
						return nil, nil, err
					}
					objField := schema.ObjectField{
						Type: propType.Encode(),
					}
					if propApiSchema.Description != "" {
						objField.Description = &propApiSchema.Description
					}
					propApiSchema.Nullable = nullable
					typeResult.Properties[propName] = *propApiSchema
					object.Fields[propName] = objField
				}

				oc.schema.ObjectTypes[refName] = object
			}
			result = schema.NewNamedType(refName)
		case "array":
			if typeSchema.Items == nil || typeSchema.Items.A == nil {
				return nil, nil, errors.New("array item is empty")
			}

			itemName := getSchemaRefTypeNameV2(typeSchema.Items.A.GetReference())
			if itemName != "" {
				result = schema.NewArrayType(schema.NewNamedType(itemName))
			} else {
				itemSchemaA := typeSchema.Items.A.Schema()
				if itemSchemaA != nil {
					itemSchema, propType, err := oc.getSchemaType(itemSchemaA, apiPath, fieldPaths)
					if err != nil {
						return nil, nil, err
					}

					typeResult.Items = propType
					result = schema.NewArrayType(itemSchema)
				}
			}

			if result == nil {
				return nil, nil, fmt.Errorf("cannot parse type reference name: %s", typeSchema.Items.A.GetReference())
			}
		default:
			return nil, nil, fmt.Errorf("unsupported schema type %s", typeName)
		}
	}

	if typeSchema.Nullable != nil && *typeSchema.Nullable {
		return schema.NewNullableType(result), typeResult, nil
	}
	return result, typeResult, nil
}

func (oc *openAPIv2Converter) convertResponse(responses *v2.Responses, apiPath string, fieldPaths []string) (schema.TypeEncoder, error) {
	if responses == nil || responses.Codes == nil || responses.Codes.IsZero() {
		return nil, nil
	}

	var resp *v2.Response
	if responses.Codes == nil || responses.Codes.IsZero() {
		// the response is alway success
		resp = responses.Default
	} else {
		for _, code := range []string{"200", "201", "204"} {
			res := responses.Codes.GetOrZero(code)
			if res != nil {
				resp = res
				break
			}
		}
	}

	// return nullable boolean type if the response content is null
	if resp == nil || resp.Schema == nil {
		return schema.NewNullableNamedType("Boolean"), nil
	}

	schemaType, _, err := oc.getSchemaTypeFromProxy(resp.Schema, false, apiPath, fieldPaths)
	if err != nil {
		return nil, err
	}
	return schemaType, nil
}

func (oc *openAPIv2Converter) convertComponentSchemas(schemaItem orderedmap.Pair[string, *base.SchemaProxy]) error {
	typeValue := schemaItem.Value()
	typeSchema := typeValue.Schema()

	if typeSchema == nil || !slices.Contains(typeSchema.Type, "object") {
		return nil
	}
	_, _, err := oc.getSchemaType(typeSchema, "", []string{schemaItem.Key()})
	return err
}

func (oc *openAPIv2Converter) trimPathPrefix(input string) string {
	if oc.ConvertOptions.TrimPrefix == "" {
		return input
	}
	return strings.TrimPrefix(input, oc.ConvertOptions.TrimPrefix)
}
