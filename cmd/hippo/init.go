package main

import (
	"flag"
	"fmt"

	"github.com/mahdi-salmanzade/hippo/web"
)

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", web.DefaultConfigPath, "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := web.InitConfig(*path)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", c.Path())
	fmt.Println("next: add provider credentials (edit the file or run `hippo serve --open`)")
	return nil
}
