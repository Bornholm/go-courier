package server

import (
	"bytes"
	"embed"
	"html/template"

	"github.com/bornholm/go-courier"
	"github.com/pkg/errors"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

//go:embed templates/*.gohtml
var embedded embed.FS

var templates *template.Template

var templateFuncs = map[string]any{
	"markdown":              markdown,
	"getMessageMainContent": courier.GetMessageMainContent,
}

func init() {
	tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(embedded, "templates/*.gohtml")
	if err != nil {
		panic(errors.Wrap(err, "could not parse templates"))
	}

	templates = tmpl
}

func markdown(text string) (template.HTML, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
		),
	)

	var buff bytes.Buffer

	if err := md.Convert([]byte(text), &buff); err != nil {
		return "", errors.WithStack(err)
	}

	return template.HTML(buff.String()), nil
}
