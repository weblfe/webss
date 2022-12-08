package main

import (
	"github.com/gin-gonic/gin"
	"github.com/weblfe/webss/assets"
	"log"
	"net/http"

	socketIo "github.com/googollee/go-socket.io"
)

func main() {
	router := gin.New()

	server := socketIo.NewServer(nil)

	server.OnConnect("/", func(s socketIo.Conn) error {
		s.SetContext("")
		log.Println("connected:", s.ID())
		return nil
	})

	server.OnEvent("/", "notice", func(s socketIo.Conn, msg string) {
		log.Println("notice:", msg)
		s.Emit("reply", "have "+msg)
	})

	server.OnEvent("/chat", "msg", func(s socketIo.Conn, msg string) string {
		s.SetContext(msg)
		log.Println(s.Namespace(), "-", msg)
		s.Emit("reply", s.Namespace()+"-"+msg)
		return "rev" + msg
	})

	server.OnEvent("/", "msg", func(s socketIo.Conn, msg string)  {
		s.SetContext(msg)
		log.Println(s.Namespace(), "-", msg)
		s.Emit("reply", s.Namespace()+"-"+msg)
	})

	server.OnEvent("/", "bye", func(s socketIo.Conn) string {
		last := s.Context().(string)
		s.Emit("bye", last)
		s.Close()
		return last
	})

	server.OnError("/", func(s socketIo.Conn, e error) {
		log.Println("meet error:", e)
	})

	server.OnDisconnect("/", func(s socketIo.Conn, reason string) {
		log.Println("closed", reason)
	})

	go func() {
		if err := server.Serve(); err != nil {
			log.Fatalf("socketIo listen error: %s\n", err)
		}
	}()
	defer server.Close()

	router.GET("/socket.io/*any", gin.WrapH(server))
	router.POST("/socket.io/*any", gin.WrapH(server))
	router.StaticFS("/public", http.FS(assets.GetFs()))

	if err := router.Run(":8000"); err != nil {
		log.Fatal("failed run app: ", err)
	}
}
