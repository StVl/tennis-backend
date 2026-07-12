package updater

import "context"

type Updater interface {
	Name() string
	Update(ctx context.Context) error
}
