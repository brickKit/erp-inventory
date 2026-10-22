// Command erp-inventory runs the inventory component alone (serve, migrate); the shells compile in inventory.Definition().
package main

import (
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/erp-inventory/v3/inventory"
)

func main() { launcher.Main(inventory.Definition()) }
