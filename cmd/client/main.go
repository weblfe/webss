package main

import (
		"bufio"
		"fmt"
		io "github.com/graarh/golang-socketio"
		"github.com/graarh/golang-socketio/transport"
		"log"
		"os"
)

type Channel struct {
		Channel string `json:"channel"`
}

type Message string

func main() {

		//addr := "ws://127.0.0.1:8000/socket.io"
		/*kvs :=io.WithQueryKvs("EIO","3",
				"user","root","password","root","transport","websocket")
		client, err := io.NewClient(io.WithAddr(addr),kvs)
		if err != nil {
				log.Printf("NewClient error:%v\n", err)
				return
		}*/
		client,err:= io.Dial(io.GetUrl("localhost", 8000, false),
				transport.GetDefaultWebsocketTransport())
		if err != nil {
				log.Printf("NewClient error:%v\n", err)
				return
		}
		client.On("error", func(h *io.Channel) {
				log.Printf("on error\n")
		})
		client.On("connection", func(h *io.Channel) {
				log.Printf("on connect\n")
		})
		client.On("msg", func(h *io.Channel, args Message) {
				log.Printf("on message:%v\n", args)
		})
		client.On("reply", func(h *io.Channel, args Message)  {
				log.Printf("on message:%v\n", args)
		})
		client.On("disconnection", func(h *io.Channel)  {
				log.Printf("on disconnect\n")
		})
		defer client.Close()
		reader := bufio.NewReader(os.Stdin)
		for {
				fmt.Print("Input: ")
				data, _, _ := reader.ReadLine()
				command := string(data)
				if command == "exit" {
						log.Printf("[exit]\n")
						return
				}
				err =client.Emit("msg", command)
				if err!=nil {
						log.Printf("[Error] %v",err)
						return
				}

				//log.Printf("send message:%v\n", command)
		}
}