package main

import (
	"fmt"
	"reflect"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LimitJoinPlugin is a GORM plugin that fixes the Limit/Offset issue with Joins/Preloads
type LimitJoinPlugin struct{}

func (p *LimitJoinPlugin) Name() string {
	return "limit_join_plugin"
}

func (p *LimitJoinPlugin) Initialize(db *gorm.DB) error {
	// Register the callback before the default query callback
	return db.Callback().Query().Before("gorm:query").Register("limit_join_plugin:before_query", func(db *gorm.DB) {
		if db.Error != nil || db.Statement.Schema == nil || db.Statement.Distinct {
			return
		}

		// Check if we are already executing the ID query to prevent infinite recursion
		if _, skip := db.InstanceGet("skip_limit_join_optimization"); skip {
			return
		}

		// Check if we have a limit or offset
		var limitVal int
		var offsetVal int
		hasLimitOrOffset := false
		if limitClause, ok := db.Statement.Clauses["LIMIT"]; ok {
			if l, ok := limitClause.Expression.(clause.Limit); ok {
				if l.Limit != nil {
					limitVal = *l.Limit
					hasLimitOrOffset = true
				}
				if l.Offset > 0 {
					offsetVal = l.Offset
					hasLimitOrOffset = true
				}
			}
		}

		hasJoins := len(db.Statement.Joins) > 0
		hasPreloads := len(db.Statement.Preloads) > 0

		// We only optimize if there is a limit/offset and we have joins or preloads
		if hasLimitOrOffset && (limitVal >= 0 || offsetVal > 0) && (hasJoins || hasPreloads) {
			// We only support single primary key for this optimization
			if len(db.Statement.Schema.PrimaryFields) != 1 {
				return
			}

			tableName := db.Statement.Table
			if tableName == "" {
				tableName = db.Statement.Schema.Table
			}

			pkField := db.Statement.Schema.PrimaryFields[0]
			pkColumn := pkField.DBName
			fqPkColumn := fmt.Sprintf("%s.%s", tableName, pkColumn)

			// Handle Limit(0) edge case
			if limitVal == 0 && limitClause, ok := db.Statement.Clauses["LIMIT"]; ok {
				if l, ok := limitClause.Expression.(clause.Limit); ok && l.Limit != nil {
					db.Statement.AddClause(clause.Where{Exprs: db.Statement.BuildConds("1 = 0")})
					delete(db.Statement.Clauses, "LIMIT")
					delete(db.Statement.Clauses, "OFFSET")
					return
				}
			}

			// Build the ID query
			idTx := db.Session(&gorm.Session{NewDB: true})
			idTx.InstanceSet("skip_limit_join_optimization", true)
			idTx.Statement.Table = db.Statement.Table
			idTx.Statement.Schema = db.Statement.Schema

			idTx.Statement.Clauses = make(map[string]clause.Clause)
			for k, v := range db.Statement.Clauses {
				idTx.Statement.Clauses[k] = v
			}
			idTx.Statement.Joins = db.Statement.Joins

			idTx.Select(fqPkColumn).Group(fqPkColumn)

			// Dynamically create a slice of the primary key type
			sliceType := reflect.SliceOf(pkField.FieldType)
			sliceVal := reflect.New(sliceType).Elem()

			err := idTx.Table(tableName).Find(sliceVal.Addr().Interface()).Error
			if err != nil {
				db.AddError(err)
				return
			}

			if sliceVal.Len() == 0 {
				db.Statement.AddClause(clause.Where{Exprs: db.Statement.BuildConds("1 = 0")})
			} else {
				db.Statement.AddClause(clause.Where{Exprs: db.Statement.BuildConds(fmt.Sprintf("%s IN (?)", fqPkColumn), sliceVal.Interface())})
			}

			delete(db.Statement.Clauses, "LIMIT")
			delete(db.Statement.Clauses, "OFFSET")
		}
	})
}

type Order struct {
	gorm.Model
	UserID uint
	Amount float64
}

type User struct {
	gorm.Model
	Name   string
	Orders []Order
}

func main() {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	// Register the plugin
	err = db.Use(&LimitJoinPlugin{})
	if err != nil {
		panic(err)
	}

	db.Migrator().DropTable(&User{}, &Order{})
	db.AutoMigrate(&User{}, &Order{})

	// Seed Data
	user1 := User{Name: "User 1", Orders: []Order{{Amount: 100}, {Amount: 200}}}
	user2 := User{Name: "User 2", Orders: []Order{{Amount: 300}}}
	user3 := User{Name: "User 3", Orders: []Order{{Amount: 400}}}
	db.Create(&user1)
	db.Create(&user2)
	db.Create(&user3)

	// Test 1: Limit(2).Preload("Orders")
	{
		var users []User
		err := db.Limit(2).Preload("Orders").Find(&users).Error
		if err != nil {
			panic(fmt.Sprintf("Test 1 failed: %v", err))
		}
		if len(users) != 2 {
			panic(fmt.Sprintf("Test 1 failed: expected 2 users, got %d", len(users)))
		}
		if users[0].Name != "User 1" || len(users[0].Orders) != 2 {
			panic(fmt.Sprintf("Test 1 failed: User 1 should have 2 orders, got %d", len(users[0].Orders)))
		}
		if users[1].Name != "User 2" || len(users[1].Orders) != 1 {
			panic(fmt.Sprintf("Test 1 failed: User 2 should have 1 order, got %d", len(users[1].Orders)))
		}
		fmt.Println("Test 1 passed!")
	}

	// Test 2: Limit(2).Joins("LEFT JOIN orders ON orders.user_id = users.id")
	{
		var users []User
		err := db.Limit(2).Joins("LEFT JOIN orders ON orders.user_id = users.id").Find(&users).Error
		if err != nil {
			panic(fmt.Sprintf("Test 2 failed: %v", err))
		}
		if len(users) != 2 {
			panic(fmt.Sprintf("Test 2 failed: expected 2 users, got %d", len(users)))
		}
		fmt.Println("Test 2 passed!")
	}

	// Test 3: Limit(0).Preload("Orders")
	{
		var users []User
		err := db.Limit(0).Preload("Orders").Find(&users).Error
		if err != nil {
			panic(fmt.Sprintf("Test 3 failed: %v", err))
		}
		if len(users) != 0 {
			panic(fmt.Sprintf("Test 3 failed: expected 0 users, got %d", len(users)))
		}
		fmt.Println("Test 3 passed!")
	}

	// Test 4: Limit(-1).Preload("Orders")
	{
		var users []User
		err := db.Limit(-1).Preload("Orders").Find(&users).Error
		if err != nil {
			panic(fmt.Sprintf("Test 4 failed: %v", err))
		}
		if len(users) != 3 {
			panic(fmt.Sprintf("Test 4 failed: expected 3 users, got %d", len(users)))
		}
		fmt.Println("Test 4 passed!")
	}

	// Test 5: Limit(1).Offset(1).Preload("Orders")
	{
		var users []User
		err := db.Limit(1).Offset(1).Preload("Orders").Find(&users).Error
		if err != nil {
			panic(fmt.Sprintf("Test 5 failed: %v", err))
		}
		if len(users) != 1 {
			panic(fmt.Sprintf("Test 5 failed: expected 1 user, got %d", len(users)))
		}
		if users[0].Name != "User 2" {
			panic(fmt.Sprintf("Test 5 failed: expected User 2, got %s", users[0].Name))
		}
		fmt.Println("Test 5 passed!")
	}

	fmt.Println("All tests passed successfully!")
}
