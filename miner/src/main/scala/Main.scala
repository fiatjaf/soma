import java.io.ByteArrayInputStream
import java.nio.charset.StandardCharsets
import scala.util.chaining._
import scala.util.control.Breaks._
import scala.util.{Try, Success, Failure}
import scala.annotation.nowarn
import scala.concurrent.ExecutionContext.Implicits.global
import scala.concurrent.{Promise, Future}
import scala.concurrent.duration._
import scala.scalanative.loop.EventLoop.loop
import scala.scalanative.loop.{Poll, Timer}
import scodec.bits.ByteVector
import ujson._
import scoin._
import unixsocket.UnixSocket

type InvoiceStatus = "holding" | "resolved" | "givenup" | "failed"

object Main {
  val logger = new nlog.Logger()

  private var rpcAddrPromise = Promise[String]()
  private var rpcAddr: Future[String] = rpcAddrPromise.future
  private var nextId = 0

  val operational = Future
    .sequence(
      List(
        Datastore.loadingPendingTransactions,
        Datastore.loadingPendingBlocks
      )
    )
    .map(_ => ())

  def main(args: Array[String]): Unit = {
    Poll(0).startRead { v =>
      var current = Array.empty[Byte]

      breakable {
        while (true) {
          // read stdin char-by-char
          Try(scala.Console.in.read()) match {
            case Success(char) if char == -1 =>
              // this will happen when stdin is closed, i.e. lightningd
              //   is not alive anymore so we should shutdown too
              scala.sys.exit(72)
            case Success(char) if char == 10 =>
              // newline, we've got a full line, so handle it
              val line = new String(current, StandardCharsets.UTF_8).trim()
              if (line.size > 0) handleRPC(line)
              current = Array.empty
            case Success(char) =>
              // normal char, add it to the current
              current = current :+ char.toByte
            case Failure(err) =>
              // EOF, stop reading and wait for the next libuv callback
              break()
          }
        }
      }
    }
  }

  def rpc(
      method: String,
      params: ujson.Obj | ujson.Arr = ujson.Obj()
  ): Future[ujson.Value] = rpcAddr.flatMap { addr =>
    nextId += 1

    val payload =
      ujson.write(
        ujson.Obj(
          "jsonrpc" -> "2.0",
          "id" -> nextId,
          "method" -> method,
          "params" -> params
        )
      )

    UnixSocket
      .call(addr, payload)
      .future
      .map(ujson.read(_))
      .flatMap(read =>
        if (read.obj.contains("error")) {
          Future.failed(Exception(read("error")("message").str))
        } else {
          Future.successful(read("result"))
        }
      )
  }

  def answer(req: ujson.Value)(result: ujson.Value): Unit = {
    System.out.println(
      ujson.write(
        ujson.Obj(
          "jsonrpc" -> "2.0",
          "id" -> req("id"),
          "result" -> result
        )
      )
    )
  }

  def answer(req: ujson.Value)(errorCode: Int, errorMessage: String): Unit = {
    System.out.println(
      ujson.write(
        ujson.Obj(
          "jsonrpc" -> "2.0",
          "id" -> req("id"),
          "error" -> ujson.Obj(
            "code" -> errorCode,
            "message" -> errorMessage
          )
        )
      )
    )
  }

  @nowarn
  def handleRPC(line: String): Unit = {
    val req = ujson.read(line)
    val params = req("params")
    def reply(result: ujson.Value) = answer(req)(result)
    def replyError(code: Int, err: String) = answer(req)(code, err)

    req("method").str match {
      case "getmanifest" =>
        reply(
          ujson.Obj(
            "dynamic" -> true,
            "options" -> ujson.Arr(),
            "hooks" -> ujson.Arr(
              ujson.Obj("name" -> "htlc_accepted")
            ),
            "rpcmethods" -> ujson.Arr(
              ujson.Obj(
                "name" -> "soma-status",
                "usage" -> "",
                "description" -> "returns a bunch of data -- this is supposed to be called by the public through commando"
              ),
              ujson.Obj(
                "name" -> "soma-invoice",
                "usage" -> "tx msatoshi",
                "description" -> "takes the transaction you want to publish plus how much in fees you intend to contribute -- this is supposed to be called by the public through commando"
              )
            ),
            "options" -> ujson.Arr(
              ujson.Obj(
                "name" -> "overseer-url",
                "type" -> "string",
                "default" -> "http://localhost:10738",
                "description" -> "URL of the soma overseer."
              ),
              ujson.Obj(
                "name" -> "node-url",
                "type" -> "string",
                "default" -> "http://127.0.0.1:9036",
                "description" -> "URL of the soma node."
              )
            ),
            "subscriptions" -> ujson.Arr("block_added"),
            "notifications" -> ujson.Arr(),
            "featurebits" -> ujson.Obj()
          )
        )
      case "init" => {
        reply(ujson.Obj())

        val lightningDir = params("configuration")("lightning-dir").str
        rpcAddrPromise.success(
          lightningDir + "/" + params("configuration")("rpc-file").str
        )
        Node.nodeUrl = params("options")("node-url").str
        Publish.overseerURL = params("options")("overseer-url").str
      }
      case "block_added" =>
        operational.foreach { _ =>
          val height = params("block")("height").num.toInt
          logger.debug.item("height", height).msg("a block has arrived")
          Manager.onBlock(height)
        }
      case "htlc_accepted" =>
        operational.foreach { _ =>
          val hash = params("htlc")("payment_hash").str
          logger.debug
            .item("hash", hash.take(6))
            .msg("invoice payment arrived")
          Manager.acceptPayment(hash).onComplete {
            case Success(true) =>
              logger.debug
                .item("hash", hash.take(6))
                .msg("fulfilling lightning payment")

              reply(ujson.Obj("result" -> "continue"))
            case Success(false) =>
              logger.debug
                .item("hash", hash.take(6))
                .msg("we couldn't include the transaction")

              reply(ujson.Obj("result" -> "fail"))
            case Failure(err) =>
              logger.err.item(err).msg("rejecting payment because of a failure")
              reply(ujson.Obj("result" -> "fail"))
          }
        }
      case "soma-status" =>
        reply(
          ujson.Obj(
            "pending_txs" -> Manager.pendingTransactions.mapValues {
              case (tx, sat, _) => ujson.Arr(tx.toHex, sat.toLong.toInt)
            },
            "last_published_txid" -> Publish.lastBitcoinTx
              .map[ujson.Value](identity)
              .getOrElse(ujson.Null),
            "last_published_block" -> Publish.lastPublishedBlock
              .map[ujson.Value](identity)
              .getOrElse(ujson.Null),
            "last_registered_block" -> Publish.lastRegisteredBlock
              .map[ujson.Value](identity)
              .getOrElse(ujson.Null)
          )
        )
      case "soma-invoice" =>
        try {
          val (tx, amount) = (params match {
            case o: Obj => (o("tx").str, o("msatoshi").num)
            case a: Arr => (a(0).str, a(1).num)
          }).pipe(v =>
            (
              ByteVector.fromValidHex(v._1),
              MilliSatoshi(v._2.toLong)
            )
          )

          Manager.makeInvoiceForTransaction(tx, amount).onComplete {
            case Success((bolt11, hash)) =>
              reply(
                ujson.Obj(
                  "invoice" -> bolt11,
                  "hash" -> hash
                )
              )
            case Failure(err) =>
              replyError(1, err.toString)
          }
        } catch {
          case err: java.util.NoSuchElementException =>
            replyError(400, "wrong params")
          case err: Throwable =>
            replyError(500, s"something went wrong: $err")
        }
    }
  }
}
