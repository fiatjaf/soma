import scala.concurrent.Future
import org.scalajs.macrotaskexecutor.MacrotaskExecutor.Implicits._
import sttp.client3._
import io.circe._
import io.circe.syntax._
import io.circe.parser._
import io.circe.generic.auto._
import scodec.bits.ByteVector
import scoin.{ByteVector32, ByteVector64}
import scoin.Crypto.{XOnlyPublicKey}
import openchain._

import JSON.given

object Node {
  def getInfo(): Future[NodeInfo] =
    call("info").map(_.as[NodeInfo].toTry.get)

  def listAllAssets(): Future[Map[String, String]] =
    call("listallassets").map(_.as[Map[String, String]].toTry.get)

  def getBmmSince(bmmHeight: Int): Future[List[BmmTx]] =
    call("getbmmsince", Map("bmmheight" -> bmmHeight.asJson))
      .map(_.as[List[BmmTx]].toTry.get)

  def getBlock(hash: String): Future[Block] =
    call("getblock", Map("hash" -> hash.asJson))
      .map(_.as[Block].toTry.get)
  // ---
  val backend = FetchBackend()
  def call(
      method: String,
      params: Map[String, Json] = Map.empty
  ): Future[io.circe.Json] =
    basicRequest
      .post(uri"http://127.0.0.1:9036/")
      .body(
        Map[String, Json](
          "method" -> method.asJson,
          "params" -> params.asJson
        ).asJson.toString
      )
      .send(backend)
      .map(_.body.toOption.get)
      .map(parse(_).toTry.get)
      .map(_.hcursor.downField("result").as[Json].toTry.get)
}

case class NodeInfo(
    latestKnownBlock: Option[BlockInfo],
    latestBmmTx: BmmTx
)

object NodeInfo {
  given Decoder[NodeInfo] = new Decoder[NodeInfo] {
    final def apply(c: HCursor): Decoder.Result[NodeInfo] = for {
      bmm <- c.downField("latest_bmm_tx").as[BmmTx]
      block = c.downField("latest_known_block").as[BlockInfo] match {
        case Right(b) => Some(b)
        case Left(_)  => None
      }
    } yield NodeInfo(block, bmm)
  }

  def empty = NodeInfo(None, BmmTx("", 0, None))
}

case class BlockInfo(hash: String, height: Int)

case class BmmTx(
    txid: String,
    bmmHeight: Int,
    bmmHash: Option[String]
)

object BmmTx {
  given Decoder[BmmTx] = new Decoder[BmmTx] {
    final def apply(c: HCursor): Decoder.Result[BmmTx] =
      for {
        txid <- c.downField("txid").as[String]
        bmmHeight <- c.downField("bmmheight").as[Int]
        bmmHash <- c.downField("bmmhash").as[Option[String]]
      } yield {
        BmmTx(txid, bmmHeight, bmmHash)
      }
  }
}

object JSON {
  given Decoder[XOnlyPublicKey] = new Decoder[XOnlyPublicKey] {
    final def apply(c: HCursor): Decoder.Result[XOnlyPublicKey] =
      c.as[ByteVector32].map(XOnlyPublicKey(_))
  }

  given Decoder[ByteVector32] = new Decoder[ByteVector32] {
    final def apply(c: HCursor): Decoder.Result[ByteVector32] =
      c.as[ByteVector].map(ByteVector32(_))
  }

  given Decoder[ByteVector64] = new Decoder[ByteVector64] {
    final def apply(c: HCursor): Decoder.Result[ByteVector64] =
      c.as[ByteVector].map(ByteVector64(_))
  }

  given Decoder[ByteVector] = new Decoder[ByteVector] {
    final def apply(c: HCursor): Decoder.Result[ByteVector] =
      c.as[String]
        .flatMap(
          ByteVector
            .fromHex(_)
            .toRight(DecodingFailure("invalid hex", c.history))
        )
  }
}