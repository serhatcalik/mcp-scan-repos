package main

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"os"
)

type Appointment struct {
	gorm.Model
	PatientID uint
}

func listAppointments(c *gin.Context) {
	c.JSON(200, []Appointment{})
}

func main() {
	_ = os.Getenv("DATABASE_URL")
	r := gin.New()
	r.GET("/appointments", listAppointments)
	_ = r.Run(":8080")
}
