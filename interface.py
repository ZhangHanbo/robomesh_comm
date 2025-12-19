#!/usr/bin/env python
import rospy
from std_msgs.msg import String
from geometry_msgs.msg import Point32
from flask import Flask, request, jsonify
import threading
import socket
import time

class WebInterface:
    def __init__(self):
        # this is the ip of go lang server
        self.target_ip = "127.0.0.1"

        # Calibrated screen coordinates from server
        self.lefttop = (0.0042,0.9500)
        self.rightbottom = (0.6553,0.0728)

        self.target_port = 8080
        self.last_robot_response = ""
        
        # Publishers and Subscribers
        self.user_instruction_pub = rospy.Publisher('/user_instruction', String, queue_size=10)
        self.user_point_pub = rospy.Publisher('/user_point', Point32, queue_size=10)
        self.robot_feedback_sub = rospy.Subscriber('/robot_feedback', String, self.robot_feedback_callback)
        
        # Flask app for HTTP requests
        self.app = Flask(__name__)
        self.setup_routes()
        
        print("VR Interface initialized")

    def compute_transformed_point(self, x, y):
        """Transform point coordinates based on calibrated screen mapping"""
        
        # Transform coordinates
        dx = (x - self.lefttop[0]) / (self.rightbottom[0] - self.lefttop[0])
        dy = (self.lefttop[1] - y) / (self.lefttop[1] - self.rightbottom[1])
        
        return dx, dy

    def extract_point_from_string(self, data_string):
        """Extract and transform point from string format 'x, y'"""
        try:
            # Split the string by comma and strip spaces
            x, y = map(float, data_string.split(","))
            # Transform the point
            x, y = self.compute_transformed_point(x, y)
            
            print(f"Transformed point: x={x}, y={y}")
            return {"x": x, "y": y}
        except Exception as e:
            print(f"Error extracting point from string: {e}")
            return None

    def validate_point(self, x, y):
        """Validate that point coordinates are within valid range [0,1]"""
        return 0 <= x <= 1 and 0 <= y <= 1

    def setup_routes(self):
        """Setup Flask routes for HTTP API"""
        
        @self.app.route('/chat', methods=['POST'])
        def chat():
            try:
                data = request.json
                user_input = data.get("text")
                if user_input:
                    print(f"Received user input: {user_input}")
                    
                    # Publish user instruction to ROS topic
                    instruction_msg = String()
                    instruction_msg.data = user_input
                    self.user_instruction_pub.publish(instruction_msg)
                    
                    # Return acknowledgment (actual response will come via socket)
                    return jsonify({"status": "received", "message": "Processing request..."})
                    
                return jsonify({"error": "No input text provided"}), 400
                
            except Exception as e:
                print(f"Error in chat endpoint: {e}")
                return jsonify({"error": "Internal server error"}), 500

        @self.app.route('/point', methods=['POST'])
        def point():
            try:
                data = request.json
                
                # Support both formats: {"x": 0.5, "y": 0.5} and {"point": "0.5, 0.5"}
                if "point" in data:
                    # Handle string format like server_web.py
                    point_string = data.get("point")
                    if point_string:
                        print(f"Received point string: {point_string}")
                        point_dict = self.extract_point_from_string(point_string)
                        if point_dict is None:
                            return jsonify({"error": "Invalid point string format"}), 400
                        x, y = point_dict["x"], point_dict["y"]
                    else:
                        return jsonify({"error": "No point string provided"}), 400
                else:
                    # Handle coordinate format
                    x = data.get("x")
                    y = data.get("y")
                    
                    if x is not None and y is not None:
                        print(f"Received point coordinates: ({x}, {y})")
                        # Apply transformation to raw coordinates
                        x, y = self.compute_transformed_point(float(x), float(y))
                        print(f"Transformed point: ({x}, {y})")
                    else:
                        return jsonify({"error": "Missing x or y coordinates"}), 400
                
                # Validate transformed coordinates
                if not self.validate_point(x, y):
                    print(f"Invalid point coordinates: ({x}, {y}) - outside [0,1] range")
                    return jsonify({"error": "Invalid point coordinates - outside valid range"}), 400
                
                # Publish point to ROS topic
                point_msg = Point32()
                point_msg.x = float(x)
                point_msg.y = float(y)
                point_msg.z = 0.0  # Not used for 2D points
                self.user_point_pub.publish(point_msg)
                
                return jsonify({
                    "status": "received", 
                    "point": [x, y],
                    "message": "Point published successfully"
                })
                
            except Exception as e:
                print(f"Error in point endpoint: {e}")
                return jsonify({"error": "Internal server error"}), 500

        @self.app.route('/health', methods=['GET'])
        def health():
            return jsonify({"status": "healthy", "service": "VR Interface"})

    def robot_feedback_callback(self, msg):
        """Callback for robot feedback - send response via socket"""
        feedback = msg.data
        if feedback and feedback != self.last_robot_response:
            self.send_via_socket(feedback)

    def send_via_socket(self, text):
        """Send text message via socket to target IP"""
        try:
            # Create a socket object
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            s.settimeout(5.0)  # 5 second timeout
            
            # Connect to the server
            s.connect((self.target_ip, self.target_port))
            
            # Send the text message
            s.sendall(text.encode('utf-8'))
            
            # Optionally, receive a response
            try:
                response = s.recv(1024)  # Buffer size of 1024 bytes
                print(f"Response from VR client: {response.decode('utf-8')}")
            except socket.timeout:
                print("No response from VR client (timeout)")
            
            # Close the socket connection
            s.close()
            
            self.last_robot_response = text
            print(f"Sent to VR client: {text}")
            
        except socket.error as e:
            print(f"Error sending text to VR client: {e}")
        except Exception as e:
            print(f"Unexpected error in send_via_socket: {e}")

    def run_flask_app(self):
        """Run the Flask app in a separate thread"""
        try:
            self.app.run(host='0.0.0.0', port=11111, debug=False, use_reloader=False)
        except Exception as e:
            print(f"Error running Flask app: {e}")

    def start(self):
        """Start the VR interface"""
        # Start Flask app in a separate thread
        flask_thread = threading.Thread(target=self.run_flask_app, daemon=True)
        flask_thread.start()
        print("VR Interface Flask server started on port 11111")
        
        # Keep the main thread alive
        try:
            rospy.spin()
        except KeyboardInterrupt:
            print("Shutting down VR Interface...")

if __name__ == "__main__":
    rospy.init_node('web_interface')
    print("Web Interface starting...")
    
    web_interface = WebInterface()
    web_interface.start()
