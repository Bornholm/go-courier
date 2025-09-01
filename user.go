package courier

type UserID string

type User interface {
	ID() UserID
	DisplayName() string
}

type BaseUser struct {
	id          UserID
	displayName string
}

// DisplayName implements User.
func (u *BaseUser) DisplayName() string {
	return u.displayName
}

// UserID implements User.
func (u *BaseUser) ID() UserID {
	return u.id
}

var _ User = &BaseUser{}

func NewUser(id UserID, displayName string) *BaseUser {
	return &BaseUser{
		id:          id,
		displayName: displayName,
	}
}
