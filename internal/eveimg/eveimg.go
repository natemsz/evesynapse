// Package eveimg builds addresses on CCP's image server.
package eveimg

import "strconv"

// base is the image server's type picture root.
const base = "https://images.evetech.net/types/"

// TypeRender is a type's 3D render (ships, drones, structures and
// other types with a model) at a size the server offers (32 to
// 1024, powers of two).
func TypeRender(typeID int64, size int) string {
	return base + strconv.FormatInt(typeID, 10) + "/render?size=" + strconv.Itoa(size)
}

// TypeIcon is a type's inventory icon, the only picture a module,
// charge or other type without a model has, at a size the server
// offers (32 or 64).
func TypeIcon(typeID int64, size int) string {
	return base + strconv.FormatInt(typeID, 10) + "/icon?size=" + strconv.Itoa(size)
}
