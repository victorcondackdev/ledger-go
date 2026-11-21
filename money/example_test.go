package money_test

import (
	"fmt"

	"github.com/victorcondackdev/ledger-go/money"
)

func ExampleParse() {
	a, err := money.Parse(money.BRL, "1234.56")
	if err != nil {
		panic(err)
	}
	fmt.Println(a.Minor(), a.Decimal())
	fmt.Println(a.Format(money.PtBR))
	fmt.Println(a.Format(money.EnUS))
	_, err = money.Parse(money.BRL, "0.001")
	fmt.Println(err)
	// Output:
	// 123456 1234.56
	// R$ 1.234,56
	// R$1,234.56
	// money: too many decimal places: "0.001"
}

func ExampleParseLocale() {
	a, _ := money.ParseLocale(money.BRL, "R$ 1.234,56", money.PtBR)
	fmt.Println(a)
	// Output: BRL 1234.56
}

// A R$ 0,05 fee shared 70/30 between the platform and a partner: the
// shares sum exactly to the fee.
func ExampleAmount_Allocate() {
	fee := money.New(5, money.BRL)
	parts, _ := fee.Allocate(70, 30)
	fmt.Println(parts[0].Format(money.PtBR), "+", parts[1].Format(money.PtBR))

	// Three installments of R$ 100,00.
	inst, _ := money.MustParse(money.BRL, "100.00").Split(3)
	fmt.Println(inst)
	// Output:
	// R$ 0,04 + R$ 0,01
	// [BRL 33.34 BRL 33.33 BRL 33.33]
}

func ExampleAmount_Add() {
	a := money.MustParse(money.BRL, "10.00")
	_, err := a.Add(money.New(100, money.USD))
	fmt.Println(err)
	_, err = money.New(1<<62, money.BRL).MulInt(4)
	fmt.Println(err)
	// Output:
	// money: currency mismatch: BRL and USD
	// money: overflow
}
