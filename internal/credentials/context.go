package credentials

import "context"

func LoadContext(ctx context.Context, s Store, email string) (Credentials, error) {
	if e := ctx.Err(); e != nil {
		return Credentials{}, e
	}
	if c, ok := s.(ContextStore); ok {
		return c.LoadContext(ctx, email)
	}
	return s.Load(email)
}
func SaveContext(ctx context.Context, s Store, email string, value Credentials) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if c, ok := s.(ContextStore); ok {
		return c.SaveContext(ctx, email, value)
	}
	return s.Save(email, value)
}

func DeleteContext(ctx context.Context, s Store, email string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if c, ok := s.(ContextStore); ok {
		return c.DeleteContext(ctx, email)
	}
	return s.Delete(email)
}
