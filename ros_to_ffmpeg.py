#!/usr/bin/env python3
"""
Stream a ROS image topic to WebRTC via RTP (VP8) using FFmpeg.

Usage:
    python ros_image_to_rtp.py /camera/rgb/image_raw

Environment (.env):
    RTP_VIDEO_IP=127.0.0.1
    RTP_VIDEO_PORT=5004
"""

import rospy
from sensor_msgs.msg import Image
import subprocess
import sys
import os
import argparse
import numpy as np

# -----------------------------
# Optional dependency: dotenv
# -----------------------------
try:
    from dotenv import load_dotenv
except ImportError:
    subprocess.check_call([sys.executable, "-m", "pip", "install", "python-dotenv"])
    from dotenv import load_dotenv

# -----------------------------
# Argument parsing
# -----------------------------
parser = argparse.ArgumentParser(description="Stream ROS image topic via RTP")
parser.add_argument(
    "topic",
    type=str,
    help="ROS image topic (e.g., /head_camera/rgb/image_raw)",
)
parser.add_argument("--fps", type=int, default=15, help="Streaming FPS (default: 15)")
args = parser.parse_args()

IMAGE_TOPIC = args.topic
FPS = args.fps

# -----------------------------
# Load environment variables
# -----------------------------
PWD = os.path.dirname(os.path.abspath(__file__))
load_dotenv(os.path.join(PWD, ".env"))

RTP_VIDEO_IP = os.getenv("RTP_VIDEO_IP", "127.0.0.1")
RTP_VIDEO_PORT = os.getenv("RTP_VIDEO_PORT", "5004")

# -----------------------------
# Global state (initialized on first frame)
# -----------------------------
ffmpeg = None
WIDTH = None
HEIGHT = None
ENCODING = None

# -----------------------------
# FFmpeg launcher
# -----------------------------
def start_ffmpeg(width, height):
    rospy.loginfo(
        f"Starting FFmpeg: {width}x{height} @ {FPS} FPS → {RTP_VIDEO_IP}:{RTP_VIDEO_PORT}"
    )

    return subprocess.Popen(
        [
            "ffmpeg",
            "-loglevel", "warning",
            "-f", "rawvideo",
            "-pix_fmt", "rgb24",
            "-s", f"{width}x{height}",
            "-r", str(FPS),
            "-i", "pipe:0",
            "-an",
            "-vcodec", "libvpx",
            "-deadline", "realtime",
            "-cpu-used", "5",
            "-g", "10",
            "-error-resilient", "1",
            "-auto-alt-ref", "1",
            "-f", "rtp",
            f"rtp://{RTP_VIDEO_IP}:{RTP_VIDEO_PORT}?pkt_size=1500",
        ],
        stdin=subprocess.PIPE,
    )

# -----------------------------
# ROS callback
# -----------------------------
def image_callback(msg):
    global ffmpeg, WIDTH, HEIGHT, ENCODING

    # Initialize on first frame
    if ffmpeg is None:
        WIDTH = msg.width
        HEIGHT = msg.height
        ENCODING = msg.encoding.lower()

        if ENCODING not in ["rgb8", "bgr8"]:
            rospy.logerr(f"Unsupported image encoding: {msg.encoding}")
            rospy.signal_shutdown("Unsupported image encoding")
            return

        ffmpeg = start_ffmpeg(WIDTH, HEIGHT)

    try:
        frame = np.frombuffer(msg.data, dtype=np.uint8).reshape(HEIGHT, WIDTH, 3)

        # Convert to RGB if necessary
        if ENCODING == "bgr8":
            frame = frame[:, :, ::-1]

        ffmpeg.stdin.write(frame.tobytes())

    except BrokenPipeError:
        rospy.logerr("FFmpeg pipe closed")
        rospy.signal_shutdown("FFmpeg pipe closed")

    except Exception as e:
        rospy.logerr(f"Streaming error: {e}")

# -----------------------------
# Main
# -----------------------------
def main():
    rospy.init_node("ros_image_rtp_streamer", anonymous=True)

    rospy.loginfo(f"Subscribing to image topic: {IMAGE_TOPIC}")
    rospy.Subscriber(IMAGE_TOPIC, Image, image_callback, queue_size=1)

    rospy.spin()

    if ffmpeg:
        ffmpeg.terminate()

if __name__ == "__main__":
    main()
