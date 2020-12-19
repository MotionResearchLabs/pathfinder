package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/andycondon/pathfinder/pkg/gps"
	"github.com/andycondon/pathfinder/pkg/ir"
	"github.com/andycondon/pathfinder/pkg/motor"
	"github.com/andycondon/pathfinder/pkg/path"
	"github.com/andycondon/pathfinder/pkg/status"
	"github.com/jacobsa/go-serial/serial"
	"periph.io/x/periph/conn/i2c"
	"periph.io/x/periph/conn/i2c/i2creg"
	"periph.io/x/periph/host"
)

func Close(closer io.Closer) {
	err := closer.Close()
	if err != nil {
		log.Println(err)
	}
}

func main() {
	if _, err := host.Init(); err != nil {
		log.Fatalf("%v", err)
	}

	// Open /dev/ttyS0 UART serial port
	serialPort, err := serial.Open(serial.OpenOptions{
		PortName:        "/dev/ttyS0",
		BaudRate:        9600,
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: 4,
	})
	if err != nil {
		log.Fatalf("serial.Open: %v", err)
	}
	defer Close(serialPort)

	// Open i2c bus #1
	bus, err := i2creg.Open("1")
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer Close(bus)

	var (
		arduino = &i2c.Dev{Addr: 0x1A, Bus: bus}
		irArray = &ir.SensorArray{
			Left:    ir.Sensor{ClearUpperBound: 0x10, FarUpperBound: 0x50},
			Forward: ir.Sensor{ClearUpperBound: 0x10, FarUpperBound: 0x50},
			Right:   ir.Sensor{ClearUpperBound: 0x10, FarUpperBound: 0x50},
		}
		statusReader = &status.Reader{Addr: 0x10, Tx: arduino.Tx, IRArray: irArray}
		m1           = &motor.Motor{Addr: 0x01, Slow: 0x50, Med: 0xA0}
		m2           = &motor.Motor{Addr: 0x02, Slow: 0x50, Med: 0xA0}
		driver       = &motor.Driver{Left: m1, Right: m2, Tx: arduino.Tx, ReadStatus: statusReader.ReadStatus}
		driverCh     = make(chan motor.Command, 100)
		irCh         = make(chan ir.Reading, 100)
		gpsCh        = make(chan gps.Reading, 100)
		errCh        = make(chan error)
		stopCh       = make(chan os.Signal, 1)
		done         = make(chan struct{})
		pathfinder   = path.Finder{Done: done, GPS: gpsCh, IR: irCh, Drive: driverCh}
		wg           sync.WaitGroup
	)

	// This is where the magic happens
	wg.Add(1)
	go func() {
		defer func() {
			log.Println("pathfinder loop done")
			wg.Done()
		}()
		pathfinder.Find()
	}()

	wg.Add(1)
	go func() {
		defer func() {
			log.Println("err loop done")
			wg.Done()
		}()
		for {
			select {
			case err := <-errCh:
				if err != nil {
					log.Printf("%v\n", err)
					// TODO Do we want to close the stop channel to end the program?
				}
			case <-done:
				return
			}
		}
	}()

	// Start the routine for reading from the ttyS0 serial port
	wg.Add(1)
	go func() {
		var (
			reader  = bufio.NewReader(serialPort)
			scanner = bufio.NewScanner(reader)
		)
		defer func() {
			log.Println("ttyS0 loop done")
			wg.Done()
		}()
		for scanner.Scan() {
			select {
			case <-done:
				return
			default:
				sentence := scanner.Text()
				if reading, err := gps.FromGPRMC(sentence); err != nil {
					errCh <- err
				} else {
					gpsCh <- reading
				}
			}
		}
	}()

	// Start the routine for I2C communication
	// Keeps all I2C communication single-threaded
	wg.Add(1)
	go func() {
		var (
			ticker      = time.NewTicker(100 * time.Millisecond)
			reading     status.Reading
			lastReading status.Reading
			err         error
		)
		defer func() {
			log.Println("i2c loop done")
			// Send a command to park so we don't drive off a cliff
			_, err = driver.D(motor.Command{M: motor.Park})
			ticker.Stop()
			wg.Done()
		}()
		for {
			select {
			case <-done:
				return
			case cmd := <-driverCh:
				_, err = driver.D(cmd)
			case <-ticker.C:
				reading, err = statusReader.Get()
				if err != nil {
					errCh <- err
					break
				}

				// TODO Add other I2C sensor checks here

				// Check individual sensors for differences, sending readings on respective channels
				// TODO Hmm, not so sure about just sending changes. More experimentation needed.
				if reading.IR != lastReading.IR {
					irCh <- reading.IR
				}
				lastReading = reading
			}
		}
	}()

	signal.Notify(stopCh, syscall.SIGTERM, syscall.SIGINT)
	<-stopCh
	log.Println("shutting down...")
	close(done)
	wg.Wait()
	log.Println("shut down complete")
}
