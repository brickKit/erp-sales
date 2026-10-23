// Command erp-sales runs the sales component alone (serve, migrate); the shells compile in sales.Definition().
package main

import (
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/erp-sales/v3/sales"
)

func main() { launcher.Main(sales.Definition()) }
